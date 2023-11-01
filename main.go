package main

import (
	"context"
	"encoding/base64"

	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/bgentry/go-netrc/netrc"
	"github.com/elazarl/goproxy"
)

func main() {
	var port int
	flag.IntVar(&port, "port", 8080, "port")

	var verbose bool
	flag.BoolVar(&verbose, "verbose", false, "set verbose")

	var listenIface string
	flag.StringVar(&listenIface, "host", "127.0.0.1", "listen interface")

	var netrcPath string
	flag.StringVar(&netrcPath, "netrc", "~/.netrc", "netrc path")

	flag.Parse()

	usr, err := user.Current()
	if err != nil {
		log.Panic(err)
	}

	if strings.HasPrefix(netrcPath, "~/") {
		dir := usr.HomeDir
		netrcPath = filepath.Join(dir, netrcPath[2:])
	}

	cmd := flag.Args()

	info := func(msg string, argv ...interface{}) {
		if verbose {
			log.Printf("INFO: "+msg, argv...)
		}
	}

	info("Loading %s", netrcPath)
	netrcFile, err := netrc.ParseFile(netrcPath)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = verbose

	// disable InsecureSkipVerify which is enabled by goproxy for some reason
	tlsConfig := proxy.Tr.TLSClientConfig.Clone()
	tlsConfig.InsecureSkipVerify = false
	proxy.Tr.TLSClientConfig = tlsConfig

	var mitmAuthHosts goproxy.FuncHttpsHandler = func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		hostname := strings.Split(host, ":")[0] // remove port
		// only MITM if it's an authenticated host, to minimuise intrusion
		if netrcFile.FindMachine(hostname) != nil {
			info("MitmConnect: %s", hostname)
			return goproxy.MitmConnect, host
		} else {
			info("OkConnect: %s", hostname)
			return goproxy.OkConnect, host
		}
	}
	proxy.OnRequest().HandleConnect(mitmAuthHosts)

	proxy.OnRequest().DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			existingAuth := r.Header.Get("authorization")
			if existingAuth != "" {
				return r, nil
			}

			machine := netrcFile.FindMachine(r.Host)
			if machine != nil {
				info("Injecting auth for %s", r.Host)
				loginStr := fmt.Sprintf("%s:%s", machine.Login, machine.Password)
				loginB64 := base64.StdEncoding.EncodeToString([]byte(loginStr))
				r.Header.Add("authorization", fmt.Sprintf("Basic %s", loginB64))
			}
			return r, nil
		})
	addr := fmt.Sprintf("%s:%d", listenIface, port)
	log.Printf("Listening on: %s", addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// done := make(chan struct{})

	server := &http.Server{
		Addr:    addr,
		Handler: proxy,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}

	// TODO listen on a random port
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	// TCP port is listening, spawn server in background
	go func() {
		err := server.Serve(listener)
		// server should never die
		log.Fatal(err)
	}()

	cacerts := fmt.Sprintf("# netproxrc self-signed root\n\n%s", string(goproxy.CA_CERT))

	systemCertPath := os.Getenv("CURL_CA_BUNDLE")
	if systemCertPath != "" {
		info("merging certificates with system bundle: %s", systemCertPath)
		systemCerts, err := os.ReadFile(systemCertPath)
		if err != nil {
			log.Panic(err)
		}
		cacerts = fmt.Sprintf("%s\n\n%s\n", systemCerts, cacerts)
	}

	certPath := filepath.Join(usr.HomeDir, ".cache", "netproxrc-cert.pem")
	err = os.WriteFile(certPath, []byte(cacerts), 0600)
	if err != nil {
		log.Panic(err)
	}
	info("Wrote CA cert to %s", certPath)

	// run command in foreground (or wait for TERM)
	if len(cmd) == 0 {
		log.Print("Press ctrl+c to terminate")
		select {}
	} else {
		exe := cmd[0]
		args := cmd[1:]

		http_proxy := fmt.Sprintf("http://localhost:%d", port)
		proc := exec.Command(exe, args...)

		for _, key := range []string{"https_proxy"} {
			envvar := fmt.Sprintf("%s=%s", key, http_proxy)
			proc.Env = append(proc.Env, envvar)
			info("+ export %s", envvar)
		}

		for _, key := range []string{"CURL_CA_BUNDLE", "SSL_CERT_FILE", "GIT_SSL_CAINFO"} {
			envvar := fmt.Sprintf("%s=%s", key, certPath)
			proc.Env = append(proc.Env, envvar)
			info("+ export %s", envvar)
		}
		info(" + %v", cmd)

		proc.Stdin = os.Stdin
		proc.Stdout = os.Stdout
		proc.Stderr = os.Stderr
		err := proc.Start()
		if err != nil {
			log.Panic(err)
		}
		err = proc.Wait()
		if err != nil {
			os.Exit(1)
		}
	}
}
