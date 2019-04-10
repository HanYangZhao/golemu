// Copyright (c) 2018 Iori Mizutani
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package main

import (
	"encoding/binary"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fatih/structs"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/iomz/go-llrp"
	"github.com/iomz/go-llrp/binutil"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	// Current Version
	version = "0.1.0"

	// app
	app                = kingpin.New("golemu", "A mock LLRP-based logical reader emulator for RFID Tags.")
	debug              = app.Flag("debug", "Enable debug mode.").Short('v').Default("false").Bool()
	initialMessageID   = app.Flag("initialMessageID", "The initial messageID to start from.").Default("1000").Int()
	initialKeepaliveID = app.Flag("initialKeepaliveID", "The initial keepaliveID to start from.").Default("80000").Int()
	ip                 = app.Flag("ip", "LLRP listening address.").Short('a').Default("0.0.0.0").IP()
	keepaliveInterval  = app.Flag("keepalive", "LLRP Keepalive interval.").Short('k').Default("0").Int()
	port               = app.Flag("port", "LLRP listening port.").Short('p').Default("5084").Int()
	pdu                = app.Flag("pdu", "The maximum size of LLRP PDU.").Short('m').Default("1500").Int()
	reportInterval     = app.Flag("reportInterval", "The interval of ROAccessReport in ms. Pseudo ROReport spec option.").Short('i').Default("10000").Int()

	// client mode
	client = app.Command("client", "Run as an LLRP client; connect to an LLRP server and receive events (test-only).")

	// server mode
	server  = app.Command("server", "Run as an LLRP tag stream server.")
	apiPort = server.Flag("apiPort", "The port for the API endpoint.").Default("3000").Int()
	file    = server.Flag("file", "The file containing Tag data.").Short('f').Default("tags.gob").String()

	// simulator mode
	simulate      = app.Command("simulate", "Run in the simulator mode.")
	simulationDir = simulate.Arg("simulationDir", "The directory contains tags for each event cycle.").Required().String()

	// LLRPConn flag
	isLLRPConnAlive = false
	// Current messageID
	messageID = uint32(*initialMessageID)
	// Current KeepaliveID
	keepaliveID = *initialKeepaliveID
	// Tag management channel
	tagManagerChannel = make(chan TagManager)
	// notify tag update channel
	notify = make(chan bool)
	// update TagReportDataStack when tag is updated
	tagUpdated = make(chan llrp.Tags)
)

// TagManager is a struct for tag management channel
type TagManager struct {
	Action ManagementAction
	Tags   llrp.Tags
}

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

// APIPostTag redirects the tag addition request
func APIPostTag(c *gin.Context) {
	var json []llrp.TagRecord
	c.BindWith(&json, binding.JSON)
	if res := ReqAddTag("add", json); res == "error" {
		c.String(http.StatusAlreadyReported, "The tag already exists!\n")
	} else {
		c.String(http.StatusAccepted, "Post requested!\n")
	}
}

// APIDeleteTag redirects the tag deletion request
func APIDeleteTag(c *gin.Context) {
	var json []llrp.TagRecord
	c.BindWith(&json, binding.JSON)
	if res := ReqDeleteTag("delete", json); res == "error" {
		c.String(http.StatusNoContent, "The tag doesn't exist!\n")
	} else {
		c.String(http.StatusAccepted, "Delete requested!\n")
	}
}

// ReqAddTag handles a tag addition request
func ReqAddTag(ut string, req []llrp.TagRecord) string {
	// TODO: success/fail notification per tag
	failed := false
	for _, t := range req {
		tag, err := llrp.NewTag(&llrp.TagRecord{
			PCBits: t.PCBits,
			EPC:    t.EPC,
		})
		if err != nil {
			log.Fatal(err)
		}

		add := TagManager{
			Action: AddTags,
			Tags:   []*llrp.Tag{tag},
		}
		tagManagerChannel <- add
	}

	if failed {
		log.Printf("failed %v %v", ut, req)
		return "error"
	}
	log.Printf("%v %v", ut, req)
	return ut
}

// ReqDeleteTag handles a tag deletion request
func ReqDeleteTag(ut string, req []llrp.TagRecord) string {
	// TODO: success/fail notification per tag
	failed := false
	for _, t := range req {
		tag, err := llrp.NewTag(&llrp.TagRecord{
			PCBits: t.PCBits,
			EPC:    t.EPC,
		})
		if err != nil {
			panic(err)
		}

		delete := TagManager{
			Action: DeleteTags,
			Tags:   []*llrp.Tag{tag},
		}
		tagManagerChannel <- delete
	}
	if failed {
		log.Printf("failed %v %v", ut, req)
		return "error"
	}
	log.Printf("%v %v", ut, req)
	return ut
}

// ReqRetrieveTag handles a tag retrieval request
func ReqRetrieveTag() []map[string]interface{} {
	retrieve := TagManager{
		Action: RetrieveTags,
		Tags:   []*llrp.Tag{},
	}
	tagManagerChannel <- retrieve
	retrieve = <-tagManagerChannel
	var tagList []map[string]interface{}
	for _, tag := range retrieve.Tags {
		t := structs.Map(llrp.NewTagRecord(*tag))
		tagList = append(tagList, t)
	}
	log.Printf("retrieve: %v", tagList)
	return tagList
}

// Handles incoming requests.
func handleRequest(conn net.Conn, tags llrp.Tags) {
	// Make a buffer to hold incoming data.
	buf := make([]byte, *pdu)
	trds := tags.BuildTagReportDataStack(*pdu)

	for {
		// Read the incoming connection into the buffer.
		reqLen, err := conn.Read(buf)
		if err == io.EOF {
			// Close the connection when you're done with it.
			log.Println("the client is disconnected, closing LLRP connection")
			conn.Close()
			return
		} else if err != nil {
			log.Println("closing LLRP connection")
			log.Print(err)
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
				log.Println("<<< SET_READER_CONFIG_RESPONSE")
			} else if header == llrp.KeepaliveAckHeader {
				// KA receieved, continue ROAR
				log.Println(">>> KEEP_ALIVE_ACK")
			}

			// Tick ROAR and Keepalive interval
			roarTicker := time.NewTicker(time.Duration(*reportInterval) * time.Millisecond)
			keepaliveTicker := &time.Ticker{}
			if *keepaliveInterval != 0 {
				keepaliveTicker = time.NewTicker(time.Duration(*keepaliveInterval) * time.Second)
			}
			go func() {
				for { // Infinite loop
					isLLRPConnAlive = true
					select {
					// ROAccessReport interval tick
					case <-roarTicker.C:
						log.Printf("<<< RO_ACCESS_REPORT (# reports: %v, # total tags: %v)", len(trds), trds.TotalTagCounts())
						for _, trd := range trds {
							roar := llrp.NewROAccessReport(trd.Data, messageID)
							err := roar.Send(conn)
							messageID++
							if err != nil {
								log.Print(err)
								isLLRPConnAlive = false
								break
							}
						}
					// Keepalive interval tick
					case <-keepaliveTicker.C:
						log.Println("<<< KEEP_ALIVE")
						conn.Write(llrp.Keepalive())
						isLLRPConnAlive = false
					// When the tag queue is updated
					case tags := <-tagUpdated:
						log.Println("### TagUpdated")
						trds = tags.BuildTagReportDataStack(*pdu)
					}
					if !isLLRPConnAlive {
						roarTicker.Stop()
						if *keepaliveInterval != 0 {
							keepaliveTicker.Stop()
						}
						break
					}
				}
			}()
		} else {
			// Unknown LLRP packet received, reset the connection
			log.Printf("unknown header: %v, reqlen: %v", header, reqLen)
			log.Printf("message: %v", buf)
			return
		}
	}
}

// server mode
func runServer() int {
	// Read virtual tags from a csv file
	log.Printf("loading virtual Tags from \"%v\"", *file)

	var tags llrp.Tags
	if _, err := os.Stat(*file); os.IsNotExist(err) {
		log.Printf("%v doesn't exist, couldn't load tags", *file)
	} else {
		err := binutil.Load(*file, &tags)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("%v tags loaded from %v", len(tags), *file)
	}

	// Listen for incoming connections.
	l, err := net.Listen("tcp", ip.String()+":"+strconv.Itoa(*port))
	if err != nil {
		panic(err)
	}

	// Close the listener when the application closes.
	defer l.Close()
	log.Printf("listening on %v:%v", ip, *port)

	// Channel for communicating virtual tag updates and signals
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	// Handle /tags
	go func() {
		r := gin.Default()
		v1 := r.Group("api/v1")
		v1.POST("/tags", APIPostTag)
		v1.DELETE("/tags", APIDeleteTag)
		r.Run(":" + strconv.Itoa(*apiPort))
	}()

	go func() {
		for {
			select {
			case cmd := <-tagManagerChannel:
				// Tag management
				res := []*llrp.Tag{}
				switch cmd.Action {
				case AddTags:
					for _, t := range cmd.Tags {
						if i := tags.GetIndexOf(t); i < 0 {
							tags = append(tags, t)
							res = append(res, t)
							// Write to file
							//writeTagsToCSV(*tags, *file)
							if isLLRPConnAlive {
								tagUpdated <- tags
							}
						}
					}
				case DeleteTags:
					for _, t := range cmd.Tags {
						if i := tags.GetIndexOf(t); i >= 0 {
							tags = append(tags[:i], tags[i+1:]...)
							res = append(res, t)
							// Write to file
							//writeTagsToCSV(tags, *file)
							if isLLRPConnAlive {
								tagUpdated <- tags
							}
						}
					}
				case RetrieveTags:
					res = tags
				}
				cmd.Tags = res
				tagManagerChannel <- cmd
			case signal := <-signals:
				// Handle SIGINT and SIGTERM.
				log.Fatalf("%v", signal)
			}
		}
	}()

	// Handle LLRP connection
	log.Println("starting LLRP connection...")
	for {
		// Accept an incoming connection.
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		log.Println("LLRP connection initiated")

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
	// sleep for 5 seconds if the host is not available and retry
	log.Printf("waiting for %s:%d ...", ip.String(), *port)
	conn, err := net.Dial("tcp", ip.String()+":"+strconv.Itoa(*port))
	for err != nil {
		time.Sleep(time.Second)
		conn, err = net.Dial("tcp", ip.String()+":"+strconv.Itoa(*port))
	}
	log.Printf("establised an LLRP connection with %v", conn.RemoteAddr())

	header := make([]byte, 2)
	length := make([]byte, 4)
	messageID := make([]byte, 4)
	for {
		_, err = io.ReadFull(conn, header)
		if err != nil {
			log.Fatal(err)
		}
		_, err = io.ReadFull(conn, length)
		if err != nil {
			log.Fatal(err)
		}
		_, err = io.ReadFull(conn, messageID)
		if err != nil {
			log.Fatal(err)
		}
		// length containts the size of the entire message in octets
		// starting from bit offset 0, hence, the message size is
		// length - 10 bytes
		var messageValue []byte
		if messageSize := binary.BigEndian.Uint32(length) - 10; messageSize != 0 {
			messageValue = make([]byte, binary.BigEndian.Uint32(length)-10)
			_, err = io.ReadFull(conn, messageValue)
			if err != nil {
				log.Fatal(err)
			}
		}

		h := binary.BigEndian.Uint16(header)
		mid := binary.BigEndian.Uint32(messageID)
		switch h {
		case llrp.ReaderEventNotificationHeader:
			log.Printf(">>> READER_EVENT_NOTIFICATION [Message ID: %d]", mid)
			conn.Write(llrp.SetReaderConfig(mid + 1))
		case llrp.KeepaliveHeader:
			log.Printf(">>> KEEP_ALIVE [Message ID: %d]", mid)
			conn.Write(llrp.KeepaliveAck())
		case llrp.SetReaderConfigResponseHeader:
			log.Printf(">>> SET_READER_CONFIG_RESPONSE [Message ID: %d]", mid)
		case llrp.ROAccessReportHeader:
			log.Printf(">>> RO_ACCESS_REPORT [Message ID: %d]", mid)
			res := llrp.UnmarshalROAccessReportBody(messageValue)
			log.Printf("%v events received", len(res))
		default:
			log.Fatalf("Unknown header: %v, Message ID: %d", h, mid)
		}
	}
}

func loadTagsForNextEventCycle(simulationFiles []string, eventCycle *int) (llrp.Tags, error) {
	tags := llrp.Tags{}
	if len(simulationFiles) <= *eventCycle {
		//log.Printf("Total iteration: %v, current event cycle: %v", len(simulationFiles), eventCycle)
		//return tags, fmt.Errorf("no more event cycle found in %s", *simulationDir)
		log.Printf("Resetting event cycle from %v to 0", *eventCycle)
		*eventCycle = 0
	}
	err := binutil.Load(simulationFiles[*eventCycle], &tags)
	if err != nil {
		return tags, err
	}
	return tags, nil
}

// simulator mode
func runSimulation() {
	// read simulation dir and prepare the file list
	dir, err := filepath.Abs(*simulationDir)
	if err != nil {
		log.Fatal(err)
	}
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}
	simulationFiles := []string{}
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".gob") {
			simulationFiles = append(simulationFiles, path.Join(dir, f.Name()))
		}
	}
	if len(simulationFiles) == 0 {
		log.Fatalf("no event cycle file found in %s", *simulationDir)
	}

	// start listening for incoming connections.
	l, err := net.Listen("tcp", ip.String()+":"+strconv.Itoa(*port))
	if err != nil {
		panic(err)
	}
	defer l.Close()
	log.Printf("listening on %v:%v", ip, *port)

	// channel for communicating virtual tag updates and signals
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for {
			select {
			case signal := <-signals:
				log.Fatal(signal)
			}
		}
	}()

	// handle LLRP connection
	log.Println("waiting for LLRP connection...")
	conn, err := l.Accept()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("initiated LLRP connection with %v", conn.RemoteAddr())

	// Send back READER_EVENT_NOTIFICATION
	currentTime := uint64(time.Now().UTC().Nanosecond() / 1000)
	conn.Write(llrp.ReaderEventNotification(messageID, currentTime))
	log.Println("<<< READER_EVENT_NOTIFICATION")
	messageID++

	// simulate event cycles from 0
	eventCycle := 0

	// initialize the first event cycle and roarTicker
	tags, err := loadTagsForNextEventCycle(simulationFiles, &eventCycle)
	if err != nil {
		log.Fatal(err)
	}
	eventCycle++
	trds := tags.BuildTagReportDataStack(*pdu)
	roarTicker := time.NewTicker(time.Duration(*reportInterval) * time.Millisecond)

	// prepare LLRP header storage
	header := make([]byte, 2)
	length := make([]byte, 4)
	receivedMessageID := make([]byte, 4)
	for {
		_, err = io.ReadFull(conn, header)
		if err != nil {
			log.Fatal(err)
		}
		_, err = io.ReadFull(conn, length)
		if err != nil {
			log.Fatal(err)
		}
		_, err = io.ReadFull(conn, receivedMessageID)
		if err != nil {
			log.Fatal(err)
		}
		var messageValue []byte
		if messageSize := binary.BigEndian.Uint32(length) - 10; messageSize != 0 {
			messageValue = make([]byte, binary.BigEndian.Uint32(length)-10)
			_, err = io.ReadFull(conn, messageValue)
			if err != nil {
				log.Fatal(err)
			}
		}

		h := binary.BigEndian.Uint16(header)
		switch h {
		case llrp.SetReaderConfigHeader:
			conn.Write(llrp.SetReaderConfigResponse())
			go func() {
				for {
					_, ok := <-roarTicker.C
					if !ok {
						log.Fatalln("roarTicker died")
					}
					log.Printf("<<< Simulated Event Cycle %v, %v tags, %v roars", eventCycle, len(tags), len(trds))
					for _, trd := range trds {
						roar := llrp.NewROAccessReport(trd.Data, messageID)
						err := roar.Send(conn)
						if err != nil {
							log.Fatal(err)
						}
						messageID++
					}
					// prepare for the next event cycle
					tags, err = loadTagsForNextEventCycle(simulationFiles, &eventCycle)
					eventCycle++
					if err != nil {
						log.Print(err)
						continue
					}
					trds = tags.BuildTagReportDataStack(*pdu)
				}
			}()
		default:
			// unknown LLRP packet received, reset the connection
			log.Printf(">>> header: %v", h)
		}
	}
}

func main() {
	app.Version(version)
	parse := kingpin.MustParse(app.Parse(os.Args[1:]))

	if *debug {
		//loggo.ConfigureLoggers("TRACE")
		gin.SetMode(gin.DebugMode)
	} else {
		//loggo.ConfigureLoggers("INFO")
		gin.SetMode(gin.ReleaseMode)
	}

	switch parse {
	case server.FullCommand():
		os.Exit(runServer())
	case client.FullCommand():
		os.Exit(runClient())
	case simulate.FullCommand():
		runSimulation()
	}
}
