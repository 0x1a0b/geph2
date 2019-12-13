package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"regexp"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/geph-official/geph2/libs/kcp-go"
	"github.com/geph-official/geph2/libs/niaucchi4"
	//"github.com/geph-official/geph2/libs/niaucchi4/backedtcp"
)

func handle(client net.Conn) {
	var err error
	defer func() {
		log.Println("Closed client", client.RemoteAddr(), "reason", err)
	}()
	defer client.Close()
	exitMatcher, err := regexp.Compile(exitRegex)
	if err != nil {
		panic(err)
	}
	dec := rlp.NewStream(client, 100000)
	for {
		var command string
		dec.Decode(&command)
		log.Println("Client", client.RemoteAddr(), "requested", command)
		switch command {
		case "tcp":
			lsnr, err := net.Listen("tcp", "")
			if err != nil {
				log.Println("cannot listen for tcp:", err)
				return
			}
			log.Println("created tcp at", lsnr.Addr())
			randokey := make([]byte, 32)
			rand.Read(randokey)
			port := lsnr.Addr().(*net.TCPAddr).Port
			rlp.Encode(client, uint(port))
			rlp.Encode(client, randokey)
			go func() {
				defer lsnr.Close()
				// var masterConn *backedtcp.Socket
				// for {
				clnt, err := lsnr.Accept()
				if err != nil {
					log.Println("error accepting", err)
					return
				}
				clnt = niaucchi4.NewObfsStream(clnt, randokey, true)
				handle(clnt)
				// go func() {
				// 	clnt.SetDeadline(time.Now().Add(time.Hour))
				// 	clnt = niaucchi4.NewObfsStream(clnt, randokey, true)
				// 	if masterConn == nil {
				// 		masterConn = backedtcp.NewSocket(clnt)
				// 		log.Println("created new master conn!")
				// 		handle(masterConn)
				// 	} else {
				// 		masterConn.Replace(clnt)
				// 	}
				// }()
				//}
			}()
			return
		case "ping":
			rlp.Encode(client, "ping")
			return
		case "ping/repeat":
			rlp.Encode(client, "ping")
		case "conn":
			fallthrough
		case "conn/feedback":
			var host string
			log.Println("waiting for host...")
			err = dec.Decode(&host)
			if err != nil {
				return
			}
			log.Println("host is", host)
			if !exitMatcher.MatchString(host) {
				err = fmt.Errorf("bad pattern: %v", host)
				return
			}
			remoteAddr := fmt.Sprintf("%v:2389", host)
			var remote net.Conn
			remote, err = net.Dial("tcp", remoteAddr)
			if err != nil {
				return
			}
			log.Println("connected to", remoteAddr)
			if command == "conn/feedback" {
				err = rlp.Encode(client, uint(0))
				if err != nil {
					log.Println("error feedbacking:", err)
					return
				}
			}
			// report stats in the background
			if statClient != nil {
				statsDone := make(chan bool)
				defer func() {
					close(statsDone)
				}()
				go func() {
					for {
						select {
						case <-statsDone:
							return
						case <-time.After(time.Millisecond * time.Duration(mrand.ExpFloat64()*3000)):
							c, ok := client.(*kcp.UDPSession)
							if ok {
								btlBw, latency, _ := c.FlowStats()
								statClient.Timing(allocGroup+".clientLatency", int64(latency))
								statClient.Timing(allocGroup+".btlBw", int64(btlBw))
							}
						}
					}
				}()
			}
			go func() {
				defer remote.Close()
				defer client.Close()
				io.Copy(remote, client)
			}()
			defer remote.Close()
			io.Copy(client, remote)
			return
		default:
			return
		}

	}
}
