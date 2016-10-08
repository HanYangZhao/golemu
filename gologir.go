// A simple LLRP-based logical reader mock for RFID Tags using go-llrp
package main

import (
	"encoding/binary"
	"io"
	"io/ioutil"
	"log"
	"net"
	//"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/iomz/go-llrp"
	"golang.org/x/net/websocket"
	"gopkg.in/alecthomas/kingpin.v2"
)

// ManagementAction is a type for TagManager
type ManagementAction int

const (
	// RetrieveTags is a const for retrieving tags
	RetrieveTags ManagementAction = iota
	// AddTags is a const for adding tags
	AddTags
	// DeleteTags is a const for deleting tags
	DeleteTags
)

// TagManager is a struct for tag management channel
type TagManager struct {
	action ManagementAction
	tags   []*Tag
}

// Constant values
const (
	// BufferSize is a general size for a buffer
	BufferSize = 512
)

var (
	// Curren Version
	version = "0.1.0"

	// kingpin app
	app = kingpin.New("gologir", "A mock LLRP-based logical reader for RFID Tags.")
	// kingpin verbose mode flag
	verbose = app.Flag("verbose", "Enable verbose mode.").Short('v').Bool()
	// kingpin initial MessageID
	initialMessageID = app.Flag("initialMessageID", "The initial messageID to start from.").Short('m').Default("1000").Int()
	// kingpin initial KeepaliveID
	initialKeepaliveID = app.Flag("initialKeepaliveID", "The initial keepaliveID to start from.").Short('k').Default("80000").Int()
	// kingpin LLRP listening port
	port = app.Flag("port", "LLRP listening port.").Short('p').Default("5084").Int()
	// kingpin LLRP listening IP address
	ip = app.Flag("ip", "LLRP listening address.").Short('i').Default("0.0.0.0").IP()

	// kingpin server command
	server = app.Command("server", "Run as a tag stream server.")
	// kingpin maximum tag to include in ROAccessReport
	maxTag = server.Flag("maxTag", "The maximum number of TagReportData parameters per ROAccessReport. Pseudo ROReport spec option. 0 for no limit.").Short('t').Default("0").Int()
	// kingpin tag list file
	file = server.Flag("file", "The file containing Tag data.").Short('f').Default("tags.csv").String()

	// kingpin client command
	client = app.Command("client", "Run as a client mode.")

	// LLRPConn flag
	isLLRPConnAlive = false
	// Current messageID
	messageID = uint32(*initialMessageID)
	// Current KeepaliveID
	keepaliveID = *initialKeepaliveID
	// Current activeClients
	activeClients = make(map[WebsockConn]int) // map containing clients
	// Tag management channel
	tagManager = make(chan *TagManager)
	// notify tag update channel
	notify = make(chan bool)
	// update TagReportDataStack when tag is updated
	tagUpdated = make(chan []*Tag)
)

func init() {
}

// Iterate through the Tags and write ROAccessReport message to the socket
func sendROAccessReport(conn net.Conn, trds *TagReportDataStack) error {
	for _, trd := range trds.Stack {
		// Append TagReportData to ROAccessReport
		roar := llrp.ROAccessReport(trd.Parameter, messageID)
		atomic.AddUint32(&messageID, 1)
		runtime.Gosched()

		// Send
		_, err := conn.Write(roar)
		if err != nil {
			return err
		}

		// Wait until ACK received
		time.Sleep(time.Millisecond)
	}

	return nil
}

// Handles incoming requests.
func handleRequest(conn net.Conn, tags []*Tag) {
	// Make a buffer to hold incoming data.
	buf := make([]byte, BufferSize)
	trds := buildTagReportDataStack(tags)

	for {
		// Read the incoming connection into the buffer.
		reqLen, err := conn.Read(buf)
		if err == io.EOF {
			// Close the connection when you're done with it.
			log.Printf("The client is disconnected, closing LLRP connection")
			conn.Close()
			return
		} else if err != nil {
			log.Println("Error:", err.Error())
			log.Printf("Closing LLRP connection")
			conn.Close()
			return
		}

		// Respond according to the LLRP packet header
		header := binary.BigEndian.Uint16(buf[:2])
		if header == llrp.SetReaderConfigHeader || header == llrp.KeepaliveAckHeader {
			if header == llrp.SetReaderConfigHeader {
				// SRC received, start ROAR
				log.Println(">>> SET_READER_CONFIG")
				conn.Write(llrp.SetReaderConfigResponse())
			} else if header == llrp.KeepaliveAckHeader {
				// KA receieved, continue ROAR
				log.Println(">>> KeepaliveAck")
			}
			// TODO: ROAR and Keepalive interval
			roarTicker := time.NewTicker(1 * time.Second)
			keepaliveTicker := time.NewTicker(10 * time.Second)
			for { // Infinite loop
				isLLRPConnAlive = true
				log.Printf("[LLRP handler select]: %v\n", trds)
				select {
				// ROAccessReport interval tick
				case <-roarTicker.C:
					log.Println("### roarTicker.C")
					log.Printf("<<< ROAccessReport(# of Tags: %v, # of TagReportData: %v\n)", trds.TotalTagCounts(), len(trds.Stack))
					err := sendROAccessReport(conn, trds)
					if err != nil {
						log.Println("Error:", err.Error())
						isLLRPConnAlive = false
					}
				// Keepalive interval tick
				case <-keepaliveTicker.C:
					log.Println("### keepaliveTicker.C")
					log.Println("<<< Keepalive")
					conn.Write(llrp.Keepalive())
					isLLRPConnAlive = false
				// When the tag queue is updated
				case tags := <-tagUpdated:
					log.Println("### TagUpdated")
					trds = buildTagReportDataStack(tags)
				}
				if !isLLRPConnAlive {
					roarTicker.Stop()
					keepaliveTicker.Stop()
					break
				}
			}
		} else {
			// Unknown LLRP packet received, reset the connection
			log.Printf("Unknown header: %v, reqlen: %v\n", header, reqLen)
			log.Printf("Message: %v\n", buf)
			return
		}
	}
}

// server mode
func runServer() int {
	// Read virtual tags from a csv file
	log.Printf("Loading virtual Tags from \"%v\"\n", *file)
	csvIn, err := ioutil.ReadFile(*file)
	check(err)
	tags := loadTagsFromCSV(string(csvIn))

	// Listen for incoming connections.
	l, err := net.Listen("tcp", ip.String()+":"+strconv.Itoa(*port))
	check(err)

	// Close the listener when the application closes.
	defer l.Close()
	log.Println("Listening on " + ip.String() + ":" + strconv.Itoa(*port))

	// Channel for communicating virtual tag updates and signals
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	// Handle websocket and static file hosting with gin
	go func() {
		r := gin.Default()
		r.Use(static.Serve("/", static.LocalFile("./public", true)))
		r.GET("/ws", func(c *gin.Context) {
			handler := websocket.Handler(SockServer)
			handler.ServeHTTP(c.Writer, c.Request)
		})
		r.Run(":8080")
	}()

	go func() {
		for {
			select {
			case cmd := <-tagManager:
				// Tag management
				res := []*Tag{}
				switch cmd.action {
				case AddTags:
					for _, t := range cmd.tags {
						if i := getIndexOfTag(tags, t); i < 0 {
							tags = append(tags, t)
							res = append(res, t)
							writeTagsToCSV(tags, *file)
							if isLLRPConnAlive {
								tagUpdated <- tags
							}
						}
					}
				case DeleteTags:
					for _, t := range cmd.tags {
						if i := getIndexOfTag(tags, t); i >= 0 {
							tags = append(tags[:i], tags[i+1:]...)
							res = append(res, t)
							writeTagsToCSV(tags, *file)
							if isLLRPConnAlive {
								tagUpdated <- tags
							}
						}
					}
				case RetrieveTags:
					res = tags
				}
				cmd.tags = res
				tagManager <- cmd
			case signal := <-signals:
				// Handle SIGINT and SIGTERM.
				log.Println(signal)
				os.Exit(0)
			}
		}
	}()

	// Handle LLRP connection
	for {
		// Accept an incoming connection.
		log.Println("LLRP connection initiated")
		conn, err := l.Accept()
		if err != nil {
			log.Println("Error:", err.Error())
			os.Exit(2)
		}

		// Send back READER_EVENT_NOTIFICATION
		currentTime := uint64(time.Now().UTC().Nanosecond() / 1000)
		conn.Write(llrp.ReaderEventNotification(messageID, currentTime))
		log.Println("<<< READER_EVENT_NOTIFICATION")
		atomic.AddUint32(&messageID, 1)
		runtime.Gosched()
		time.Sleep(time.Millisecond)

		// Handle connections in a new goroutine.
		go handleRequest(conn, tags)
	}
}

// client mode
func runClient() int {
	// Establish a connection to the llrp client
	conn, err := net.Dial("tcp", ip.String()+":"+strconv.Itoa(*port))
	check(err)

	buf := make([]byte, BufferSize)
	for {
		// Read the incoming connection into the buffer.
		reqLen, err := conn.Read(buf)
		if err == io.EOF {
			// Close the connection when you're done with it.
			return 0
		} else if err != nil {
			log.Println("Error:", err.Error())
			log.Printf("reqLen = %v\n", reqLen)
			conn.Close()
			break
		}

		header := binary.BigEndian.Uint16(buf[:2])
		if header == llrp.ReaderEventNotificationHeader {
			log.Println(">>> READER_EVENT_NOTIFICATION")
			conn.Write(llrp.SetReaderConfig(messageID))
		} else if header == llrp.SetReaderConfigResponseHeader {
			log.Println(">>> SET_READER_CONFIG_RESPONSE")
		} else if header == llrp.ROAccessReportHeader {
			log.Println(">>> RO_ACCESS_REPORT")
			log.Printf("Packet size: %v\n", reqLen)
			log.Printf("% x\n", buf[:reqLen])
		} else {
			log.Printf("Unknown header: %v\n", header)
		}
	}
	return 0
}

func main() {
	app.Version(version)
	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case server.FullCommand():
		os.Exit(runServer())
	case client.FullCommand():
		os.Exit(runClient())
	}
}
