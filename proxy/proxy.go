package proxy

import (
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/kyoukaya/rhine/log"
	"github.com/kyoukaya/rhine/proxy/filters"
	"github.com/kyoukaya/rhine/utils"

	"github.com/elazarl/goproxy"
)

// Options optionally changes the behavior of the proxy
type Options struct {
	Logger      log.Logger // Defaults to log.Log if not specified
	LoggerFlags int        // Flags to pass to the standard logger, if a custom logger is not specified
	// LogPath defaults to "logs/proxy.log", setting it to "/dev/null", even on Windows,
	// will make the logger not output a file.
	LogPath          string
	LogDisableStdOut bool           // Should stdout output be DISABLED for the default logger
	EnableHostFilter bool           // Filters out packets from certain hosts if they match HostFilter
	HostFilter       *regexp.Regexp // Custom regexp filter for filtering packets, defaults to the block list in proxy/filters.go
	Verbose          bool           // log more Rhine information
	VerboseGoProxy   bool           // log every GoProxy request to stdout
	Address          string         // proxy listen address, defaults to ":8080"
}

// regionMap maps the TLD of the host string to their constant regional
// representation: ["GL", "JP"]
var regionMap = map[string]string{
	"global": "GL",
	"jp":     "JP",
}

// Proxy contains the internal state relevant to the proxy
type Proxy struct {
	mutex      *sync.Mutex
	server     *goproxy.ProxyHttpServer
	hostFilter *regexp.Regexp
	options    *Options
	// users contains a mapping of a user's UID and region in string form to a
	// Dispatch struct containing the context pertaining to the user.
	users map[string]*Dispatch
	log.Logger
}

// Interface shim for goproxy.Logger
func logShim(logger log.Logger) func(format string, v ...interface{}) {
	return func(format string, v ...interface{}) {
		logger.Infof(format, v...)
	}
}

const (
	certPath = "/cert.pem"
	keyPath  = "/key.pem"
)

type printfFunc func(format string, v ...interface{})

func (f printfFunc) Printf(format string, v ...interface{}) {
	f("[goproxy] "+format, v...)
}

// NewProxy returns a new initialized Dispatch
func NewProxy(options *Options) *Proxy {
	logger := options.Logger
	if logger == nil {
		logger = log.New(!options.LogDisableStdOut, options.Verbose, options.LogPath, options.LoggerFlags)
	}
	if options.Address == "" {
		options.Address = ":8080"
	}
	var proxyFilter *regexp.Regexp = nil
	if options.EnableHostFilter {
		if options.HostFilter == nil {
			proxyFilter = filters.HostFilter
		} else {
			proxyFilter = options.HostFilter
		}
	}
	server := goproxy.NewProxyHttpServer()
	server.Logger = printfFunc(logShim(logger))
	server.Verbose = options.VerboseGoProxy
	proxy := &Proxy{
		mutex:      &sync.Mutex{},
		server:     server,
		options:    options,
		Logger:     logger,
		users:      make(map[string]*Dispatch),
		hostFilter: proxyFilter,
	}
	server.OnRequest().DoFunc(proxy.HandleReq)
	server.OnResponse().DoFunc(proxy.HandleResp)

	_, certStatErr := os.Stat(utils.BinDir + certPath)
	_, keyStatErr := os.Stat(utils.BinDir + keyPath)
	// Generate CA if it doesn't exist
	if os.IsNotExist(certStatErr) || os.IsNotExist(keyStatErr) {
		proxy.Infof("Generating CA...")
		if err := utils.GenerateCA(certPath, keyPath); err != nil {
			proxy.Fatal(err)
		}
		proxy.Infof("Copy and register the created 'cert.pem' with your client.")
	}
	if err := utils.LoadCA(certPath, keyPath); err != nil {
		proxy.Fatal(err)
	}

	server.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(proxy.httpsHandler))
	return proxy
}

// HTTPSHandler to allow HTTPS connections to pass through the proxy without being
// MITM'd.
func (p *Proxy) httpsHandler(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if p.hostFilter != nil && p.hostFilter.MatchString(host) {
		p.Verbosef("==== Rejecting %v", host)
		return goproxy.RejectConnect, host
	}
	return goproxy.MitmConnect, host
}

// Start starts the proxy. This is blocking and does not return.
func (proxy *Proxy) Start() {
	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	// Catch sigint/sigterm and cleanly exit
	go func() {
		<-sigs
		proxy.Infof("Shutting down.\n")
		proxy.Flush()
		proxy.Shutdown()
		os.Exit(0)
	}()

	ipstring := utils.GetOutboundIP()
	addrSplit := strings.Split(proxy.options.Address, ":")
	if len(addrSplit) == 2 {
		ipstring += ":" + addrSplit[1]
	}
	proxy.Infof("proxy server listening on %s", ipstring)
	proxy.Fatal(http.ListenAndServe(proxy.options.Address, proxy.server))
}

// Shutdown calls Shutdown on all modules for all users.
func (proxy *Proxy) Shutdown() {
	for _, user := range proxy.users {
		for _, cb := range user.shutdownCBs {
			cb(true)
		}
	}
}

// GetUser returns a Dispatch for the specified UID
func (proxy *Proxy) GetUser(UID, region string) *Dispatch {
	rUID := region + "_" + UID
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	return proxy.users[rUID]
}

// AddUser records a user's information indexed by their UID, if a record belonging to
// the specified UID already exists, its hooks will be shutdown and the record will be overwritten.
func (proxy *Proxy) AddUser(UID, region string) *Dispatch {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	rUID := region + "_" + UID

	if user, exists := proxy.users[rUID]; exists {
		proxy.Infof("%s reconnecting. Shutting down mods.", rUID)
		for _, cb := range user.shutdownCBs {
			cb(false)
		}
	} else {
		proxy.Infof("User %s logged in", rUID)
	}

	UIDint, err := strconv.Atoi(UID)
	utils.Check(err)
	d := &Dispatch{
		mutex:  &sync.Mutex{},
		UID:    UIDint,
		Region: region,
		Hooks:  make(map[string][]*PacketHook),
		Logger: proxy.Logger,
	}
	d.initMods(modules)
	proxy.users[rUID] = d
	return d
}
