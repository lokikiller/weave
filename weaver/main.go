package main

import (
	"github.com/google/gopacket/layers"
	"crypto/sha256"
	"flag"
	"fmt"
	"github.com/davecheney/profile"
	"github.com/gorilla/mux"
	. "github.com/weaveworks/weave/common"
	"github.com/weaveworks/weave/common/updater"
	"github.com/weaveworks/weave/ipam"
	weavenet "github.com/weaveworks/weave/net"
	weave "github.com/weaveworks/weave/router"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
)

var version = "(unreleased version)"

func main() {

	log.SetPrefix(weave.Protocol + " ")
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	procs := runtime.NumCPU()
	// packet sniffing can block an OS thread, so we need one thread
	// for that plus at least one more.
	if procs < 2 {
		procs = 2
	}
	runtime.GOMAXPROCS(procs)

	var (
		config      weave.RouterConfig
		justVersion bool
		ifaceName   string
		routerName  string
		nickName    string
		password    string
		wait        int
		debug       bool
		pktdebug    bool
		prof        string
		peers       []string
		bufSzMB     int
		httpAddr    string
		iprangeCIDR string
		peerCount   int
		apiPath     string
	)

	flag.BoolVar(&justVersion, "version", false, "print version and exit")
	flag.IntVar(&config.Port, "port", weave.Port, "router port")
	flag.StringVar(&ifaceName, "iface", "", "name of interface to capture/inject from (disabled if blank)")
	flag.StringVar(&routerName, "name", "", "name of router (defaults to MAC of interface)")
	flag.StringVar(&nickName, "nickname", "", "nickname of peer (defaults to hostname)")
	flag.StringVar(&password, "password", "", "network password")
	flag.IntVar(&wait, "wait", 0, "number of seconds to wait for interface to be created and come up (0 = don't wait)")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.BoolVar(&pktdebug, "pktdebug", false, "enable per-packet debug logging")
	flag.StringVar(&prof, "profile", "", "enable profiling and write profiles to given path")
	flag.IntVar(&config.ConnLimit, "connlimit", 30, "connection limit (0 for unlimited)")
	flag.IntVar(&bufSzMB, "bufsz", 8, "capture buffer size in MB")
	flag.StringVar(&httpAddr, "httpaddr", fmt.Sprintf(":%d", weave.HTTPPort), "address to bind HTTP interface to (disabled if blank, absolute path indicates unix domain socket)")
	flag.StringVar(&iprangeCIDR, "iprange", "", "IP address range to allocate within, in CIDR notation")
	flag.IntVar(&peerCount, "initpeercount", 0, "number of peers in network (for IP address allocation)")
	flag.StringVar(&apiPath, "api", "unix:///var/run/docker.sock", "Path to Docker API socket")
	flag.Parse()
	peers = flag.Args()

	InitDefaultLogging(debug)
	if justVersion {
		fmt.Printf("weave router %s\n", version)
		os.Exit(0)
	}

	log.Println("Command line options:", options())
	log.Println("Command line peers:", peers)

	var err error

	if ifaceName != "" {
		config.Iface, err = weavenet.EnsureInterface(ifaceName, wait)
		if err != nil {
			log.Fatal(err)
		}
	}

	if routerName == "" {
		if config.Iface == nil {
			log.Fatal("Either an interface must be specified with -iface or a name with -name")
		}
		routerName = config.Iface.HardwareAddr.String()
	}
	name, err := weave.PeerNameFromUserInput(routerName)
	if err != nil {
		log.Fatal(err)
	}

	if nickName == "" {
		nickName, err = os.Hostname()
		if err != nil {
			log.Fatal(err)
		}
	}

	if password == "" {
		password = os.Getenv("WEAVE_PASSWORD")
	}

	if password == "" {
		log.Println("Communication between peers is unencrypted.")
	} else {
		config.Password = []byte(password)
		log.Println("Communication between peers is encrypted.")
	}

	if prof != "" {
		p := *profile.CPUProfile
		p.ProfilePath = prof
		p.NoShutdownHook = true
		defer profile.Start(&p).Stop()
	}

	config.BufSz = bufSzMB * 1024 * 1024
	config.LogFrame = logFrameFunc(pktdebug)

	router := weave.NewRouter(config, name, nickName)
	log.Println("Our name is", router.Ourself)

	var allocator *ipam.Allocator
	if iprangeCIDR != "" {
		allocator = createAllocator(router, apiPath, iprangeCIDR, determineQuorum(peerCount, peers))
	} else if peerCount > 0 {
		log.Fatal("-initpeercount flag specified without -iprange")
	} else {
		router.NewGossip("IPallocation", &ipam.DummyAllocator{})
	}

	router.Start()
	initiateConnections(router, peers)

	// The weave script always waits for a status call to succeed,
	// so there is no point in doing "weave launch -httpaddr ''".
	// This is here to support stand-alone use of weaver.
	if httpAddr != "" {
		go handleHTTP(router, httpAddr, allocator)
	}

	SignalHandlerLoop(router)
}

func options() map[string]string {
	options := make(map[string]string)
	flag.Visit(func(f *flag.Flag) {
		value := f.Value.String()
		if f.Name == "password" {
			value = "<elided>"
		}
		options[f.Name] = value
	})
	return options
}

func logFrameFunc(debug bool) weave.LogFrameFunc {
	if !debug {
		return func(prefix string, frame []byte, eth *layers.Ethernet) {}
	}
	return func(prefix string, frame []byte, eth *layers.Ethernet) {
		h := fmt.Sprintf("%x", sha256.Sum256(frame))
		if eth == nil {
			log.Println(prefix, len(frame), "bytes (", h, ")")
		} else {
			log.Println(prefix, len(frame), "bytes (", h, "):", eth.SrcMAC, "->", eth.DstMAC)
		}
	}
}

func initiateConnections(router *weave.Router, peers []string) {
	for _, peer := range peers {
		if err := router.ConnectionMaker.InitiateConnection(peer); err != nil {
			log.Fatal(err)
		}
	}
}

func createAllocator(router *weave.Router, apiPath string, iprangeCIDR string, quorum uint) *ipam.Allocator {
	allocator, err := ipam.NewAllocator(router.Ourself.Peer.Name, router.Ourself.Peer.UID, router.Ourself.Peer.NickName, iprangeCIDR, quorum)
	if err != nil {
		log.Fatal(err)
	}
	allocator.SetInterfaces(router.NewGossip("IPallocation", allocator))
	allocator.Start()
	err = updater.Start(apiPath, allocator)
	if err != nil {
		log.Fatal("Unable to start watcher", err)
	}
	return allocator
}

// Pick a quorum size heuristically based on the number of peer
// addresses passed.
func determineQuorum(initPeerCountFlag int, peers []string) uint {
	if initPeerCountFlag > 0 {
		return uint(initPeerCountFlag/2 + 1)
	}

	// Guess a suitable quorum size based on the list of peer
	// addresses.  The peer list might or might not contain an
	// address for this peer, so the conservative assumption is
	// that it doesn't.  The list might contain multiple addresses
	// that resolve to the same peer, in which case the quorum
	// might be larger than it needs to be.  But the user can
	// specify it explicitly if that becomes a problem.
	clusterSize := uint(len(peers) + 1)
	quorum := clusterSize/2 + 1
	log.Println("Assuming quorum size of", quorum)
	return quorum
}

func handleHTTP(router *weave.Router, httpAddr string, allocator *ipam.Allocator) {
	encryption := "off"
	if router.UsingPassword() {
		encryption = "on"
	}

	muxRouter := mux.NewRouter()

	if allocator != nil {
		allocator.HandleHTTP(muxRouter)
	}

	muxRouter.Methods("GET").Path("/status").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "weave router", version)
		fmt.Fprintln(w, "Encryption", encryption)
		fmt.Fprintln(w, router.Status())
		if allocator != nil {
			fmt.Fprintln(w, allocator.String())
		}
	})

	muxRouter.Methods("GET").Path("/status-json").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json, _ := router.StatusJSON(version, encryption)
		w.Write(json)
	})

	muxRouter.Methods("POST").Path("/connect").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := router.ConnectionMaker.InitiateConnection(r.FormValue("peer")); err != nil {
			http.Error(w, fmt.Sprint("invalid peer address: ", err), http.StatusBadRequest)
		}
	})

	muxRouter.Methods("POST").Path("/forget").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ConnectionMaker.ForgetConnection(r.FormValue("peer"))
	})

	http.Handle("/", muxRouter)

	protocol := "tcp"
	if strings.HasPrefix(httpAddr, "/") {
		os.Remove(httpAddr) // in case it's there from last time
		protocol = "unix"
	}
	l, err := net.Listen(protocol, httpAddr)
	if err != nil {
		log.Fatal("Unable to create http listener socket: ", err)
	}

	err = http.Serve(l, nil)
	if err != nil {
		log.Fatal("Unable to create http server", err)
	}
}
