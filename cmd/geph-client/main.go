package main

import (
	"flag"
	"io/ioutil"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/bunsim/goproxy"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/geph-official/geph2/libs/bdclient"
	"github.com/geph-official/geph2/libs/cwl"
	"github.com/geph-official/geph2/libs/tinysocks"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

var username string
var password string
var ticketFile string
var binderFront string
var binderHost string
var exitName string
var exitKey string
var forceBridge bool
var loginCheck bool

var direct bool

var socksAddr string
var statsAddr string

var bindClient *bdclient.Client

var sWrap *muxWrap

func main() {
	mrand.Seed(time.Now().UnixNano())
	// flags
	flag.StringVar(&username, "username", "pwtest", "username")
	flag.StringVar(&password, "password", "pwtest", "password")
	flag.StringVar(&ticketFile, "ticketFile", "", "location for caching auth tickets")
	flag.StringVar(&binderFront, "binderFront", "https://ajax.aspnetcdn.com/v2", "front location of binder")
	flag.StringVar(&binderHost, "binderHost", "gephbinder.azureedge.net", "true hostname of binder")
	flag.StringVar(&exitName, "exitName", "us-sfo-01.exits.geph.io", "qualified name of the exit node selected")
	flag.StringVar(&exitKey, "exitKey", "2f8571e4795032433098af285c0ce9e43c973ac3ad71bf178e4f2aaa39794aec", "ed25519 pubkey of the selected exit")
	flag.BoolVar(&forceBridge, "forceBridge", false, "force the use of obfuscated bridges")
	flag.BoolVar(&loginCheck, "loginCheck", false, "do a login check and immediately exit with code 0")
	flag.StringVar(&socksAddr, "socksAddr", "localhost:9909", "SOCKS5 listening address")
	flag.StringVar(&statsAddr, "statsAddr", "localhost:9809", "HTTP listener for statistics")
	flag.Parse()

	runtime.GOMAXPROCS(1)

	if loginCheck {
		go func() {
			time.Sleep(time.Second * 10)
			os.Exit(-1)
		}()
	}

	// connect to bridge
	bindClient = bdclient.NewClient(binderFront, binderHost)
	sWrap = newSmuxWrapper()

	// automatically pick mode
	if !forceBridge {
		country, err := bindClient.GetClientInfo()
		if err != nil {
			log.Println("cannot get country, conservatively using bridges", err)
		} else {
			log.Println("country is", country.Country)
			if country.Country == "CN" {
				log.Println("in CHINA, must use bridges")
			} else {
				log.Println("disabling bridges")
				direct = true
			}
		}
	}

	// spin up stats server
	http.HandleFunc("/", handleStats)
	go func() {
		err := http.ListenAndServe(statsAddr, nil)
		if err != nil {
			panic(err)
		}
	}()

	// confirm we are connected
	func() {
		rm, _ := sWrap.DialCmd("ip")
		defer rm.Close()
		var ip string
		err := rlp.Decode(rm, &ip)
		if err != nil {
			log.Println("Uh oh, cannot get IP!")
			os.Exit(404)
		}
		ip = strings.TrimSpace(ip)
		log.Println("Successfully got external IP", ip)
		useStats(func(sc *stats) {
			sc.Connected = true
			sc.PublicIP = ip
		})
		if loginCheck {
			os.Exit(0)
		}
	}()

	// HTTP proxy
	srv := goproxy.NewProxyHttpServer()
	srv.Tr = &http.Transport{
		Dial: func(n, d string) (net.Conn, error) {
			return dialTun(d)
		},
		IdleConnTimeout: time.Second * 60,
		Proxy:           nil,
	}
	srv.Logger = log.New(ioutil.Discard, "", 0)
	go func() {
		err := http.ListenAndServe("127.0.0.1:8780", srv)
		if err != nil {
			panic(err.Error())
		}
	}()

	listenLoop()
}

func dialTun(dest string) (conn net.Conn, err error) {
	sks, err := proxy.SOCKS5("tcp", "127.0.0.1:9909", nil, proxy.Direct)
	if err != nil {
		return
	}
	conn, err = sks.Dial("tcp", dest)
	return
}

func listenLoop() {
	listener, err := net.Listen("tcp", socksAddr)
	if err != nil {
		panic(err)
	}
	log.Println("SOCKS5 on 9909")
	for {
		cl, err := listener.Accept()
		if err != nil {
			panic(err)
		}
		go func() {
			defer cl.Close()
			rmAddr, err := tinysocks.ReadRequest(cl)
			if err != nil {
				return
			}
			start := time.Now()
			remote, ok := sWrap.DialCmd("proxy", rmAddr)
			defer remote.Close()
			log.Printf("opened %v in %v", rmAddr, time.Since(start))
			if !ok {
				tinysocks.CompleteRequest(5, cl)
				return
			}
			tinysocks.CompleteRequest(0, cl)
			go func() {
				defer remote.Close()
				defer cl.Close()
				cwl.CopyWithLimit(remote, cl, rate.NewLimiter(rate.Inf, 10000000), func(n int) {
					useStats(func(sc *stats) {
						sc.UpBytes += uint64(n)
					})
				})
			}()
			cwl.CopyWithLimit(cl, remote,
				rate.NewLimiter(rate.Inf, 10000000), func(n int) {
					useStats(func(sc *stats) {
						sc.DownBytes += uint64(n)
					})
				})
		}()
	}
}
