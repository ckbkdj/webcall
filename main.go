// WebCall Copyright 2022 timur.mobi. All rights reserved.
//
// WebCall server is a signaling server for WebRTC clients.
// It's main task is to connect two clients, so that they
// can establish a peer-to-peer connection.
//
// main.go calls readConfig(), opens the database files,
// opens the websocket handlers for ws and wss communication,
// starts the httpServer(), the turnServer() and a couple of
// background processes (tickers). The server will run until 
// it receives a SIGTERM event. It will then run the shutdown
// procedure.
//
// Clients connect via XHR requests in httpServer.go.
// Callee clients will then be managed by httpLogin.go. 
// Caller clients will be managed by httpOnline.go.
// Both clients will then switch to the websocket protocol 
// (wsClient.go).
// On successful callee login a wsHub object is created.
// From that point forward the callee can receive calls.
// A wsHub object holds two wsClient objects for callee and
// caller. When a call comes in, wsClient takes care of the 
// signaling process.

package main

import (
	"flag"
	"net/http"
	"fmt"
	"time"
	"os"
	"os/signal"
	"syscall"
	"sync"
	"sync/atomic"
	"strings"
	"strconv"
	"bufio"
	"runtime"
	"math/rand"
	"gopkg.in/ini.v1"

	"bytes"
	"encoding/gob"
	bolt "go.etcd.io/bbolt"

	_ "net/http/pprof"
	"github.com/mehrvarz/webcall/atombool"
	"github.com/mehrvarz/webcall/iptools"
	"github.com/mehrvarz/webcall/skv"
	"github.com/lesismal/nbio/nbhttp"
	"github.com/lesismal/llib/std/crypto/tls"
)

var	kvMain skv.KV
const dbMainName = "rtcsig.db"
const dbRegisteredIDs = "activeIDs"
const dbBlockedIDs = "blockedIDs"
const dbUserBucket = "userData2"	// see dbObjects.go

var	kvCalls skv.KV
const dbCallsName = "rtccalls.db"
const dbWaitingCaller = "waitingCallers"
const dbMissedCalls = "missedCalls"
type CallerInfo struct {
	AddrPort string
	CallerName string
	CallTime int64
	CallerID string
	Msg string
}

var	kvContacts skv.KV
const dbContactsName = "rtccontacts.db"
const dbContactsBucket = "contacts" // calleeID -> map[callerID]name

var	kvHashedPw skv.KV
const dbHashedPwName = "rtchashedpw.db"
const dbHashedPwBucket = "hashedpwbucket"
type PwIdCombo struct {
	Pw string
	CalleeId string
	Created int64
	Expiration int64
}


var version = flag.Bool("version", false, "show version")
var	builddate string
var	codetag string
const configFileName = "config.ini"
const statsFileName = "stats.ini"
var readConfigLock sync.RWMutex
var	shutdownStarted atombool.AtomBool
var queryFollowerIDsNeeded atombool.AtomBool

var hubMap map[string]*Hub
var hubMapMutex sync.RWMutex

// ws-connect timeout blocker
// blockMap lets us temp-block an ip in response to a ws-reconnect issue (likely caused by battery optimization)
var blockMap map[string]time.Time
var blockMapMutex sync.RWMutex

// calleeLoginMap contains an array of timestamps of previous login attempts
// this lets us limit the number of login attempts to maxLoginPer30min (e.g. 30)
var calleeLoginMap map[string][]time.Time
var calleeLoginMutex sync.RWMutex
var maxLoginPer30min = 0 // ideal value 9 - 12

// clientRequestsMap contains an array of timestamps of previous http requests
// this lets us limit the number of http requests to maxClientRequestsPer30min (e.g. 180)
var clientRequestsMap map[string][]time.Time // ip -> []time.Time
var clientRequestsMutex sync.RWMutex
var maxClientRequestsPer30min = 0 // ideal value 60

// remoteAddr listed in missedCallAllowedMap are temporarily eligible to send xhr /missedCall
var missedCallAllowedMap map[string]time.Time
var missedCallAllowedMutex sync.RWMutex

var waitingCallerChanMap map[string]chan int // ip:port -> chan
var waitingCallerChanLock sync.RWMutex

type MappingDataType struct {
	CalleeId string
	Assign string
}
var mapping map[string]MappingDataType
var mappingMutex sync.RWMutex

// newsDateMap[calleeID] returns the datestring of the last news.ini delivery
var newsDateMap map[string]string
//var newsDateMutex sync.RWMutex

var numberOfCallsToday = 0 // will be incremented by wshub.go processTimeValues()
var numberOfCallSecondsToday int64 = 0
var numberOfCallsTodayMutex sync.RWMutex

var lastCurrentDayOfMonth = 0 // will be set by timer.go
var wsAddr string
var wssAddr string
var svr *nbhttp.Server
var svrs *nbhttp.Server

var mastodonMgr *MastodonMgr

type wsClientDataType struct {
	hub *Hub
	dbEntry DbEntry
	dbUser DbUser
	calleeID string
	globalID string
	dialID string
	clientVersion string
	removeFlag bool
}
// wsClientMap[wsid] contains wsClientDataType at the moment of a callee login
var wsClientMap map[uint64]wsClientDataType
var wsClientMutex sync.RWMutex
var pingSentCounter int64 = 0
var pongSentCounter int64 = 0
var outboundIP = ""


// config keywords: must be evaluated with readConfigLock
var hostname = ""
var httpPort = 0
var httpsPort = 0
var httpToHttps = false
var wsPort = 0
var wssPort = 0
var htmlPath = ""
var insecureSkipVerify = false
var turnIP = ""
var turnPort = 0
var turnRealm = ""
var turnDebugLevel = 0
var pprofPort = 0
var dbPath = ""
var wsUrl = ""
var wssUrl = ""
var mastodonhandler = ""
var twitterKey = ""
var twitterSecret = ""
var vapidPublicKey = ""
var vapidPrivateKey = ""
var timeLocationString = ""
var timeLocation *time.Location = nil
var maintenanceMode = false
var allowNewAccounts = true
var multiCallees = ""
var logevents = ""
var logeventMap map[string]bool
var logeventMutex sync.RWMutex
//var disconCalleeOnPeerConnected = false
var disconCallerOnPeerConnected = true
var maxRingSecs = 0
var maxTalkSecsIfNoP2p = 0
var adminID = ""
var adminEmail = ""
var	backupScript = ""
var	backupPauseMinutes = 0
var maxCallees = 0
var cspString = ""
var thirtySecStats = false
var clientUpdateBelowVersion = ""
var clientBlockBelowVersion = ""
var serverStartTime time.Time
var adminLogPath1 = ""
var adminLogPath2 = ""


func main() {
	flag.Parse()
	if *version {
		if codetag!="" {
			fmt.Printf("version %s\n",codetag)
		}
		fmt.Printf("builddate %s\n",builddate)
		return
	}

	fmt.Printf("----- webcall %s %s startup -----\n", codetag, builddate)
	serverStartTime = time.Now()
	hubMap = make(map[string]*Hub) // calleeID -> *Hub
	blockMap = make(map[string]time.Time)
	calleeLoginMap = make(map[string][]time.Time)
	clientRequestsMap = make(map[string][]time.Time)
	missedCallAllowedMap = make(map[string]time.Time)
	waitingCallerChanMap = make(map[string]chan int)
	mapping = make(map[string]MappingDataType)
	newsDateMap = make(map[string]string)

	wsClientMap = make(map[uint64]wsClientDataType) // wsClientID -> wsClientData

	readConfig(true) // need dbPath

	var err error
	kvMain,err = skv.DbOpen(dbMainName,dbPath)
	if err!=nil {
		fmt.Printf("# error DbOpen %s path %s err=%v\n",dbMainName,dbPath,err)
		return
	}
	err = kvMain.CreateBucket(dbRegisteredIDs)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbMainName,dbRegisteredIDs,err)
		kvMain.Close()
		return
	}
	err = kvMain.CreateBucket(dbBlockedIDs)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbMainName,dbBlockedIDs,err)
		kvMain.Close()
		return
	}
	err = kvMain.CreateBucket(dbUserBucket)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbMainName,dbUserBucket,err)
		kvMain.Close()
		return
	}
	kvCalls,err = skv.DbOpen(dbCallsName,dbPath)
	if err!=nil {
		fmt.Printf("# error DbOpen %s path %s err=%v\n",dbCallsName,dbPath,err)
		return
	}
	err = kvCalls.CreateBucket(dbWaitingCaller)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbCallsName,dbWaitingCaller,err)
		kvCalls.Close()
		return
	}
	err = kvCalls.CreateBucket(dbMissedCalls)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbCallsName,dbMissedCalls,err)
		kvCalls.Close()
		return
	}

	kvHashedPw,err = skv.DbOpen(dbHashedPwName,dbPath)
	if err!=nil {
		fmt.Printf("# error DbOpen %s path %s err=%v\n",dbHashedPwName,dbPath,err)
		return
	}
	err = kvHashedPw.CreateBucket(dbHashedPwBucket)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbHashedPwName,dbHashedPwBucket,err)
		kvHashedPw.Close()
		return
	}
	kvContacts,err = skv.DbOpen(dbContactsName,dbPath)
	if err!=nil {
		fmt.Printf("# error DbOpen %s path %s err=%v\n",dbContactsName,dbPath,err)
		return
	}
	err = kvContacts.CreateBucket(dbContactsBucket)
	if err!=nil {
		fmt.Printf("# error db %s CreateBucket %s err=%v\n",dbContactsName,dbContactsBucket,err)
		kvContacts.Close()
		return
	}

	rand.Seed(time.Now().UnixNano())
	queryFollowerIDsNeeded.Set(true)

	outboundIP,err = iptools.GetOutboundIP()
	fmt.Printf("outboundIP %s\n",outboundIP)

	// init mapping from dbUserBucket
	kv := kvMain.(skv.SKV)
	db := kv.Db
	skv.DbMutex.Lock()
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(dbUserBucket))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			// k = ID ("calleeid_1619008491")
			// v = dbUser []byte

			calleeID := string(k)
			idxUline := strings.Index(calleeID,"_")
			if idxUline>= 0 {
				calleeID = calleeID[:idxUline]
			}

			var dbUser DbUser // DbEntry{unixTime, remoteAddr, urlPw}
			d := gob.NewDecoder(bytes.NewReader(v))
			d.Decode(&dbUser)
			if dbUser.AltIDs!="" {
				//fmt.Printf("initloop %s (%s)->%s\n",k,calleeID,dbUser.AltIDs)
				toks := strings.Split(dbUser.AltIDs, "|")
				for tok := range toks {
					toks2 := strings.Split(toks[tok], ",")
					if toks2[0] != "" { // tmpID
						if toks2[1] == "true" {
							fmt.Printf("initloop set mapping from AltIDs %s -> %s (%s)\n",
								toks2[0], calleeID, toks2[2])
							mapping[toks2[0]] = MappingDataType{calleeID,toks2[2]}
						}
					}
				}
			}

			if dbUser.MastodonID!="" {
				fmt.Printf("initloop set mapping from MastodonID %s -> %s\n", dbUser.MastodonID, calleeID)
				mapping[dbUser.MastodonID] = MappingDataType{calleeID,"none"}
			}
		}
		return nil
	})
	skv.DbMutex.Unlock()

	readConfig(false) // if configured start mastodonhandler

	readStatsFile()

	// websocket handler
	if wsPort > 0 {
		wsAddr = fmt.Sprintf(":%d", wsPort)
		mux := &http.ServeMux{}
		mux.HandleFunc("/ws", serveWs)
		svr = nbhttp.NewServer(nbhttp.Config{
			Network: "tcp",
			Addrs: []string{wsAddr},
			MaxLoad: 1000000,				// TODO make configurable?
			ReleaseWebsocketPayload: true,	// TODO make configurable?
			NPoller: runtime.NumCPU() * 4,	// TODO make configurable? user workers?
		}, mux, nil)
		err = svr.Start()
		if err != nil {
			fmt.Printf("# nbio.Start wsPort failed: %v\n", err)
			return
		}
		defer svr.Stop()
	}
	if wssPort>0 {
		cer, err := tls.LoadX509KeyPair("tls.pem", "tls.key")
		if err != nil {
			fmt.Printf("# tls.LoadX509KeyPair err=(%v)\n", err)
			os.Exit(-1)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cer},
			InsecureSkipVerify: insecureSkipVerify,
			// Causes servers to use Go's default ciphersuite preferences,
			// which are tuned to avoid attacks. Does nothing on clients.
			PreferServerCipherSuites: true,
			// Only use curves which have assembly implementations
			CurvePreferences: []tls.CurveID{
				tls.CurveP256,
				tls.X25519,
			},
			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			},
		}
		tlsConfig.BuildNameToCertificate()
		//fmt.Printf("tlsConfig %v\n", tlsConfig)

		wssAddr = fmt.Sprintf(":%d", wssPort)
		mux := &http.ServeMux{}
		mux.HandleFunc("/ws", serveWss)
		svrs = nbhttp.NewServerTLS(nbhttp.Config{
			Network: "tcp",
			Addrs: []string{wssAddr},
			MaxLoad: 1000000,				// TODO make configurable?
			ReleaseWebsocketPayload: true,	// TODO make configurable?
			NPoller: runtime.NumCPU() * 4,	// TODO make configurable? user workers?
		}, mux, nil, tlsConfig)
		err = svrs.Start()
		if err != nil {
			fmt.Printf("# nbio.Start wssPort failed: %v\n", err)
			return
		}
		defer svrs.Stop()
	}

	go httpServer()
	go runTurnServer()
	go ticker3hours()  // check time since last login
	go ticker20min()   // update news notifieer
	go ticker3min()    // backupScript
	go ticker30sec()   // log stats
	go ticker10sec()   // readConfig(false)
	go ticker2sec()    // check for new day
	if pprofPort>0 {
		go func() {
			addr := fmt.Sprintf(":%d",pprofPort)
			fmt.Printf("starting pprofServer on %s\n",addr)
			pprofServer := &http.Server{Addr:addr}
			pprofServer.ListenAndServe()
		}()
	}

	time.Sleep(1 * time.Second)
	fmt.Printf("awaiting SIGTERM for shutdown...\n")
	sigc := make(chan os.Signal)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc

	// shutdown
	fmt.Printf("received os.Interrupt/SIGTERM signal: shutting down...\n")
	// shutdownStarted.Set(true) will end all timer routines
	// but it will not end ListenAndServe() servers; this is why we call os.Exit() below
	shutdownStarted.Set(true)
	writeStatsFile()
	time.Sleep(2 * time.Second)

	fmt.Printf("kvContacts.Close...\n")
	err = kvContacts.Close()
	if err!=nil {
		fmt.Printf("# error dbName %s close err=%v\n",dbContactsName,err)
	}
	fmt.Printf("kvHashedPw.Close...\n")
	err = kvHashedPw.Close()
	if err!=nil {
		fmt.Printf("# error dbName %s close err=%v\n",dbHashedPwName,err)
	}
	fmt.Printf("kvCalls.Close...\n")
	err = kvCalls.Close()
	if err!=nil {
		fmt.Printf("# error dbName %s close err=%v\n",dbCallsName,err)
	}
	fmt.Printf("db.Close...\n")
	err = kvMain.Close()
	if err!=nil {
		fmt.Printf("# error dbName %s close err=%v\n",dbMainName,err)
	}
	if mastodonMgr != nil {
		mastodonMgr.mastodonStop()
		mastodonMgr = nil
	}

	skv.Exit()
	os.Exit(0)
}

// getStats() creates a string with live info about the number of 
// callees, callers (the number of current calls), how many are p2p,
// and the total number of calls and call seconds since midnight
func getStats() string {
	var numberOfOnlineCallees int64
	var numberOfOnlineCallers int64
	numberOfActivePureP2pCalls := 0
	hubMapMutex.RLock()
	defer hubMapMutex.RUnlock()
	for _,hub := range hubMap {
		if hub!=nil {
			numberOfOnlineCallees++
//			hub.HubMutex.RLock()
			if hub.lastCallStartTime>0 /*&& hub.CallerClient!=nil*/ {
				numberOfOnlineCallers++
				if hub.LocalP2p && hub.RemoteP2p {
					numberOfActivePureP2pCalls++
				}
			}
//			hub.HubMutex.RUnlock()
		}
	}
	//hubMapMutex.RUnlock()

	numberOfCallsTodayMutex.RLock()
	retStr := fmt.Sprintf("stats %d/%d/%d calls:%d/%d %d/%d go:%d",
		numberOfOnlineCallees, numberOfOnlineCallers, numberOfActivePureP2pCalls,
		numberOfCallsToday, numberOfCallSecondsToday, // fed by hub.processTimeValues()
		atomic.LoadInt64(&pingSentCounter), atomic.LoadInt64(&pongSentCounter),
		runtime.NumGoroutine())
	numberOfCallsTodayMutex.RUnlock()
	return retStr
}

// if timeLocationString is specified, operationalNow() will return
// the current time for the given location
// this is useful if your server is hosted in a timezone diffrent 
// than you.
func operationalNow() time.Time {
	if timeLocationString!="" {
		if timeLocation == nil {
			loc, err := time.LoadLocation(timeLocationString)
			if err != nil {
				panic(err)
			}
			timeLocation = loc
		}
		return time.Now().In(timeLocation)
	}
	return time.Now()
}

// logWantedFor(), together with the logevents config keyword, 
// allows for topic specific logging
func logWantedFor(topic string) bool {
	logeventMutex.RLock()
	if logeventMap[topic] {
		logeventMutex.RUnlock()
		return true
	}
	logeventMutex.RUnlock()
	return false
}

// readConfig() supports two types of config keywords
// those that are only evaluated once during startup (see "init")
// and those that are evaluated every time readConfig() is called
func readConfig(init bool) {
	//fmt.Printf("readConfig '%s' ...\n", configFileName)
	configIni, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true,},configFileName)
	if err != nil {
		// ignore the read error and instead use the default values
		configIni = nil
	}

	mastodonhandlerNew := ""

	readConfigLock.Lock()
	if init {
		hostname = readIniString(configIni, "hostname", hostname, "127.0.0.1")
		httpPort = readIniInt(configIni, "httpPort", httpPort, 8067, 1)
		httpsPort = readIniInt(configIni, "httpsPort", httpsPort, 0, 1)
		httpToHttps = readIniBoolean(configIni, "httpToHttps", httpToHttps, false)
		wsPort = readIniInt(configIni, "wsPort", wsPort, 8071, 1)
		wssPort = readIniInt(configIni, "wssPort", wssPort, 0, 1)
		htmlPath = readIniString(configIni, "htmlPath", htmlPath, "")
		insecureSkipVerify = readIniBoolean(configIni, "insecureSkipVerify", insecureSkipVerify, false)
		turnIP = readIniString(configIni, "turnIP", turnIP, "")
		turnPort = readIniInt(configIni, "turnPort", turnPort, 0, 1) // 3739
		turnRealm = readIniString(configIni, "turnRealm", turnRealm, "")
		pprofPort = readIniInt(configIni, "pprofPort", pprofPort, 0, 1) // 8980
		dbPath = readIniString(configIni, "dbPath", dbPath, "db/")
		if dbPath!="" && !strings.HasSuffix(dbPath,"/") { dbPath = dbPath+"/" }
		timeLocationString = readIniString(configIni, "timeLocation", timeLocationString, "")
		wsUrl = readIniString(configIni, "wsUrl", wsUrl, "")
		wssUrl = readIniString(configIni, "wssUrl", wssUrl, "")

		twitterKey = readIniString(configIni, "twitterKey", twitterKey, "")
		twitterSecret = readIniString(configIni, "twitterSecret", twitterSecret, "")

		// currently not used
		vapidPublicKey = readIniString(configIni, "vapidPublicKey", vapidPublicKey, "")
		vapidPrivateKey = readIniString(configIni, "vapidPrivateKey", vapidPrivateKey, "")
//		readConfigLock.Unlock()
//		return
	}

	maintenanceMode = readIniBoolean(configIni, "maintenanceMode", maintenanceMode, false)
	allowNewAccounts = readIniBoolean(configIni, "allowNewAccounts", allowNewAccounts, true)

	multiCallees = readIniString(configIni, "multiCallees", multiCallees, "")

	logevents = readIniString(configIni, "logevents", logevents, "")
	logeventSlice := strings.Split(logevents, ",")
	logeventMutex.Lock()
	logeventMap = make(map[string]bool)
	for _, s := range logeventSlice {
		logeventMap[strings.TrimSpace(s)] = true
	}
	logeventMutex.Unlock()

//	disconCalleeOnPeerConnected = readIniBoolean(configIni,
//		"disconCalleeOnPeerConnected", disconCalleeOnPeerConnected, false)
	disconCallerOnPeerConnected = readIniBoolean(configIni,
		"disconCallerOnPeerConnected", disconCallerOnPeerConnected, true)

	maxRingSecs = readIniInt(configIni, "maxRingSecs", maxRingSecs, 120, 1)
	maxTalkSecsIfNoP2p = readIniInt(configIni, "maxTalkSecsIfNoP2p", maxTalkSecsIfNoP2p, 600, 1)

	turnDebugLevel = readIniInt(configIni, "turnDebugLevel", turnDebugLevel, 3, 1)

	adminID = readIniString(configIni, "adminID", adminID, "")
	adminEmail = readIniString(configIni, "adminEmail", adminEmail, "")
	adminLogPath1 = readIniString(configIni, "adminLog1", adminLogPath1, "")
	adminLogPath2 = readIniString(configIni, "adminLog2", adminLogPath2, "")

	backupScript = readIniString(configIni, "backupScript", backupScript, "")
	backupPauseMinutes = readIniInt(configIni, "backupPauseMinutes", backupPauseMinutes, 720, 1)

	maxCallees = readIniInt(configIni, "maxCallees", maxCallees, 10000, 1)

	cspString = readIniString(configIni, "csp", cspString, "")

	thirtySecStats = readIniBoolean(configIni, "thirtySecStats", thirtySecStats, false)

	clientUpdateBelowVersion = readIniString(configIni, "clientUpdateBelowVersion", clientUpdateBelowVersion, "")
	clientBlockBelowVersion  = readIniString(configIni, "clientBlockBelowVersion", clientBlockBelowVersion, "")

	maxLoginPer30min = readIniInt(configIni, "maxLoginPer30min", maxLoginPer30min, 0, 1)
	maxClientRequestsPer30min = readIniInt(configIni, "maxRequestsPer30min", maxClientRequestsPer30min, 0, 1)

	mastodonhandlerNew = readIniString(configIni, "mastodonhandler", mastodonhandler, "")
	readConfigLock.Unlock()

	if !init {
		if mastodonhandlerNew != mastodonhandler {
			//fmt.Printf("config.ini mastodonhandler '%s' <- '%s'\n", mastodonhandlerNew, mastodonhandler)
			mastodonhandler = mastodonhandlerNew
			if mastodonhandler=="" {
				if mastodonMgr != nil {
					//fmt.Printf("mastodonStop\n")
					mastodonMgr.mastodonStop()
					mastodonMgr = nil
				}
			} else {
				if mastodonMgr == nil {
					//fmt.Printf("mastodonStart...\n")
					mastodonMgr = NewMastodonMgr()
					go mastodonMgr.mastodonStart(mastodonhandler)
				}
			}
		}
	}
}

// readStatsFile() reads a file "stats.ini" in which some information
// ("numberOfCallsToday". "numberOfCallSecondsToday", "lastCurrentDayOfMonth")
// is kept persisted, so that this info is still available after a restart
func readStatsFile() {
	statsIni, err := ini.Load(statsFileName)
	if err != nil {
		//fmt.Printf("# cannot read ini file '%s', err=%v\n", statsFileName, err)
		// we ignore this; WebCall works fine without a statsFile
		return
	}

	iniKeyword := "numberOfCallsToday"
	iniValue,ok := readIniEntry(statsIni,iniKeyword)
	if ok {
		if iniValue=="" {
			numberOfCallsToday = 0
		} else {
			i64, err := strconv.ParseInt(iniValue, 10, 64)
			if err!=nil {
				fmt.Printf("# stats val %s: %s=%v err=%v\n",
					statsFileName, iniKeyword, iniValue, err)
			} else {
				//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
				numberOfCallsToday = int(i64)
			}
		}
	}

	iniKeyword = "numberOfCallSecondsToday"
	iniValue,ok = readIniEntry(statsIni,iniKeyword)
	if ok {
		if iniValue=="" {
			numberOfCallsToday = 0
		} else {
			i64, err := strconv.ParseInt(iniValue, 10, 64)
			if err!=nil {
				fmt.Printf("# stats val %s: %s=%v err=%v\n",
					statsFileName, iniKeyword, iniValue, err)
			} else {
				//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
				numberOfCallSecondsToday = i64
			}
		}
	}

	iniKeyword = "lastCurrentDayOfMonth"
	iniValue,ok = readIniEntry(statsIni,iniKeyword)
	if ok {
		if iniValue=="" {
			lastCurrentDayOfMonth = 0
		} else {
			i64, err := strconv.ParseInt(iniValue, 10, 64)
			if err!=nil {
				fmt.Printf("# stats val %s: %s=%v err=%v\n",
					statsFileName, iniKeyword, iniValue, err)
			} else {
				//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
				lastCurrentDayOfMonth = int(i64)
			}
		}
	}
}

// writeStatsFile() writes the file read by readStatsFile()
func writeStatsFile() {
	filename := statsFileName
	os.Remove(filename)
	file,err := os.OpenFile(filename,os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("# error creating statsFile (%s) err=%v\n", filename, err)
		return
	}
	defer func() {
		if file!=nil {
			if err := file.Close(); err != nil {
				fmt.Printf("# error closing statsFile (%s) err=%s\n",filename,err)
			}
		}
	}()
	fwr := bufio.NewWriter(file)
	defer func() {
		if fwr!=nil {
			fwr.Flush()
		}
	}()

	numberOfCallsTodayMutex.RLock()
	data := fmt.Sprintf("numberOfCallsToday = %d\n"+
						"numberOfCallSecondsToday = %d\n"+
						"lastCurrentDayOfMonth = %d\n",
		numberOfCallsToday, numberOfCallSecondsToday, lastCurrentDayOfMonth)
	numberOfCallsTodayMutex.RUnlock()
	wrlen,err := fwr.WriteString(data)
	if err != nil {
		fmt.Printf("# error writing statsFile (%s) data err=%s\n", filename, err)
		return
	}
	if wrlen != len(data) {
		fmt.Printf("# error writing statsFile (%s) dlen=%d wrlen=%d\n",
			filename, len(data), wrlen)
		return
	}
	fmt.Printf("statsFile written (%s) dlen=%d wrlen=%d\n", filename, len(data), wrlen)
	fwr.Flush()
}

