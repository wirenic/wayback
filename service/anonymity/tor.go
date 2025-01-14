// Copyright 2020 Wayback Archiver. All rights reserved.
// Use of this source code is governed by the GNU GPL v3
// license that can be found in the LICENSE file.

package anonymity // import "github.com/wabarc/wayback/service/anonymity"

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/cretz/bine/tor"
	"github.com/cretz/bine/torutil/ed25519"
	// "github.com/ipsn/go-libtor"
	"github.com/wabarc/helper"
	"github.com/wabarc/logger"
	"github.com/wabarc/wayback"
	"github.com/wabarc/wayback/config"
	"github.com/wabarc/wayback/errors"
	"github.com/wabarc/wayback/publish"
	"github.com/wabarc/wayback/template"
)

type Tor struct {
}

// New tor struct.
func New() *Tor {
	return &Tor{}
}

// Serve accepts incoming HTTP requests over Tor network, or open
// a local port for proxy server by "WAYBACK_TOR_LOCAL_PORT" env.
// Use "WAYBACK_TOR_PRIVKEY" to keep the Tor hidden service hostname.
//
// Serve always returns an error.
func (t *Tor) Serve(ctx context.Context) error {
	// Start tor with some defaults + elevated verbosity
	logger.Info("[web] starting and registering onion service, please wait a bit...")

	if _, err := exec.LookPath("tor"); err != nil {
		logger.Fatal("%v", err)
	}

	var pvk ed25519.PrivateKey
	if config.Opts.TorPrivKey() == "" {
		keypair, _ := ed25519.GenerateKey(rand.Reader)
		pvk = keypair.PrivateKey()
		logger.Info("[web] important to keep the private key: %s", hex.EncodeToString(pvk))
	} else {
		privb, err := hex.DecodeString(config.Opts.TorPrivKey())
		if err != nil {
			logger.Fatal("[web] the key %s is not specific", err)
		}
		pvk = ed25519.PrivateKey(privb)
	}

	verbose := config.Opts.HasDebugMode()
	// startConf := &tor.StartConf{ProcessCreator: libtor.Creator, DataDir: "tor-data"}
	startConf := &tor.StartConf{TorrcFile: t.torrc(), TempDataDirBase: os.TempDir()}
	if verbose {
		startConf.DebugWriter = os.Stdout
	} else {
		startConf.ExtraArgs = []string{"--quiet"}
	}
	e, err := tor.Start(ctx, startConf)
	if err != nil {
		logger.Fatal("[web] failed to start tor: %v", err)
	}
	defer e.Close()

	// Create an onion service to listen on any port but show as local port,
	// specify the local port using the `WAYBACK_TOR_LOCAL_PORT` environment variable.
	onion, err := e.Listen(ctx, &tor.ListenConf{LocalPort: config.Opts.TorLocalPort(), RemotePorts: config.Opts.TorRemotePorts(), Version3: true, Key: pvk})
	if err != nil {
		logger.Fatal("[web] failed to create onion service: %v", err)
	}
	defer onion.Close()

	logger.Info(`[web] listening on %q without TLS`, onion.LocalListener.Addr())
	logger.Info("[web] please open a Tor capable browser and navigate to http://%v.onion", onion.ID)

	go func() {
		http.HandleFunc("/", home)
		http.HandleFunc("/w", func(w http.ResponseWriter, r *http.Request) { t.process(w, r, ctx) })
		http.Serve(onion, nil)
	}()

	stop := make(chan os.Signal)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	return errors.New("done")
}

func home(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Collector{}
	if html, ok := tmpl.Render(); ok {
		w.Write(html)
	} else {
		logger.Error("[web] render template for home request failed")
		http.Error(w, "Internal Server Error", 500)
	}
}

func (t *Tor) process(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	logger.Debug("[web] process request start...")
	if r.Method != http.MethodPost {
		logger.Info("[web] request method no specific.")
		http.Redirect(w, r, "/", 405)
		return
	}

	if err := r.ParseForm(); err != nil {
		logger.Error("[web] parse form error, %v", err)
		http.Redirect(w, r, "/", 400)
		return
	}

	text := r.PostFormValue("text")
	if len(strings.TrimSpace(text)) == 0 {
		logger.Info("[web] post form value empty.")
		http.Redirect(w, r, "/", 411)
		return
	}

	logger.Debug("[web] text: %s", text)

	collector, col := t.archive(ctx, text)
	switch r.PostFormValue("data-type") {
	case "json":
		w.Header().Set("Content-Type", "application/json")

		if data, err := json.Marshal(collector); err != nil {
			logger.Error("[web] encode for response failed, %v", err)
		} else {
			go publish.To(ctx, col, "web")
			w.Write(data)
		}

		return
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if html, ok := collector.Render(); ok {
			go publish.To(ctx, col, "web")
			w.Write(html)
		} else {
			logger.Error("[web] render template for response failed")
		}

		return
	}
}

func (t *Tor) archive(ctx context.Context, text string) (tc *template.Collector, col []*wayback.Collect) {
	logger.Debug("[web] archives start...")
	tc = &template.Collector{}

	urls := helper.MatchURL(text)
	if len(urls) == 0 {
		transform(tc, "", map[string]string{text: "URL no found"})
		logger.Info("[web] archives failure, URL no found.")
		return tc, []*wayback.Collect{}
	}

	wg := sync.WaitGroup{}
	var wbrc wayback.Broker = &wayback.Handle{URLs: urls}
	for slot, arc := range config.Opts.Slots() {
		if !arc {
			continue
		}
		wg.Add(1)
		logger.Debug("[web] archiving slot: %s", slot)
		go func(slot string, tc *template.Collector) {
			defer wg.Done()
			slotName := config.SlotName(slot)
			c := &wayback.Collect{
				Arc: slotName,
				Ext: config.SlotExtra(slot),
			}
			switch slot {
			case config.SLOT_IA:
				ia := wbrc.IA()
				// Data for response
				transform(tc, slotName, ia)
				// Data for publish
				c.Dst = ia
			case config.SLOT_IS:
				is := wbrc.IS()
				// Data for response
				transform(tc, slotName, is)
				// Data for publish
				c.Dst = is
			case config.SLOT_IP:
				ip := wbrc.IP()
				// Data for response
				transform(tc, slotName, ip)
				// Data for publish
				c.Dst = ip
			case config.SLOT_PH:
				ph := wbrc.PH()
				// Data for response
				transform(tc, slotName, ph)
				// Data for publish
				c.Dst = ph
			}
			col = append(col, c)
		}(slot, tc)
	}
	wg.Wait()

	return tc, col
}

func transform(c *template.Collector, slot string, arc map[string]string) {
	p := *c
	for src, dst := range arc {
		p = append(p, template.Collect{Slot: slot, Src: src, Dst: dst})
	}
	*c = p
}

func (t *Tor) torrc() string {
	if config.Opts.TorrcFile() == "" {
		return ""
	}
	if _, err := os.Open(config.Opts.TorrcFile()); err != nil {
		return ""
	}
	return config.Opts.TorrcFile()
}
