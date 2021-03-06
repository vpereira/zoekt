// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"
	"github.com/google/zoekt/web"
)

const logFormat = "2006-01-02T15-04-05.999999999Z07"

// To be set from the linker.
var Version string

func divertLogs(dir string, interval time.Duration) {
	t := time.NewTicker(interval)
	var last *os.File
	for {
		nm := filepath.Join(dir, fmt.Sprintf("zoekt-webserver.%s.%d.log", time.Now().Format(logFormat), os.Getpid()))
		fmt.Fprintf(os.Stderr, "writing logs to %s\n", nm)

		f, err := os.Create(nm)
		if err != nil {
			// There is not much we can do now.
			fmt.Fprintf(os.Stderr, "can't create output file %s: %v\n", nm, err)
			os.Exit(2)
		}

		log.SetOutput(f)
		last.Close()

		last = f

		<-t.C
	}
}

const templateExtension = ".html.tpl"

func loadTemplates(tpl *template.Template, dir string) error {
	fs, err := filepath.Glob(dir + "/*" + templateExtension)
	if err != nil {
		log.Fatalf("Glob: %v", err)
	}

	for _, fn := range fs {
		content, err := ioutil.ReadFile(fn)
		if err != nil {
			return err
		}

		base := filepath.Base(fn)
		base = strings.TrimSuffix(base, ".html")
		if _, err := tpl.New(base).Parse(string(content)); err != nil {
			return fmt.Errorf("Parse(%s): %v", fn, err)
		}
	}
	return nil
}

func writeTemplates(dir string) error {
	if dir == "" {
		return fmt.Errorf("must set --template_dir")
	}

	for k, v := range web.TemplateText {
		nm := filepath.Join(dir, k+templateExtension)
		if err := ioutil.WriteFile(nm, []byte(v), 0644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	logDir := flag.String("log_dir", "", "log to this directory rather than stderr.")
	logRefresh := flag.Duration("log_refresh", 24*time.Hour, "if using --log_dir, start writing a new file this often.")

	listen := flag.String("listen", ":6070", "listen on this address.")
	index := flag.String("index", build.DefaultDir, "set index directory to use")
	html := flag.Bool("html", true, "enable HTML interface")
	restAPI := flag.Bool("rest_api", false, "enable REST API")
	print := flag.Bool("print", false, "enable local result URLs")
	enablePprof := flag.Bool("pprof", false, "set to enable remote profiling.")
	sslCert := flag.String("ssl_cert", "", "set path to SSL .pem holding certificate.")
	sslKey := flag.String("ssl_key", "", "set path to SSL .pem holding key.")
	hostCustomization := flag.String(
		"host_customization", "",
		"specify host customization, as HOST1=QUERY,HOST2=QUERY")

	templateDir := flag.String("template_dir", "", "set directory from which to load custom .html.tpl template files")
	dumpTemplates := flag.Bool("dump_templates", false, "dump templates into --template_dir and exit.")

	flag.Parse()

	if *dumpTemplates {
		if err := writeTemplates(*templateDir); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if *logDir != "" {
		if fi, err := os.Lstat(*logDir); err != nil || !fi.IsDir() {
			log.Fatal("%s is not a directory", *logDir)
		}
		// We could do fdup acrobatics to also redirect
		// stderr, but it is simpler and more portable for the
		// caller to divert stderr output if necessary.
		go divertLogs(*logDir, *logRefresh)
	}

	searcher, err := zoekt.NewShardedSearcher(*index)
	if err != nil {
		log.Fatal(err)
	}

	s := &web.Server{
		Searcher: searcher,
		Top:      web.Top,
		Version:  Version,
	}

	if *templateDir != "" {
		if err := loadTemplates(s.Top, *templateDir); err != nil {
			log.Fatal("loadTemplates: %v", err)
		}
	}

	s.Print = *print
	s.HTML = *html
	s.RESTAPI = *restAPI

	if *hostCustomization != "" {
		s.HostCustomQueries = map[string]string{}
		for _, h := range strings.SplitN(*hostCustomization, ",", -1) {
			if len(h) == 0 {
				continue
			}
			fields := strings.SplitN(h, "=", 2)
			if len(fields) < 2 {
				log.Fatal("invalid host_customization %q", h)
			}

			s.HostCustomQueries[fields[0]] = fields[1]
		}
	}

	handler, err := web.NewMux(s)
	if err != nil {
		log.Fatal(err)
	}

	if *enablePprof {
		handler.HandleFunc("/debug/pprof/", pprof.Index)
		handler.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		handler.HandleFunc("/debug/pprof/profile", pprof.Profile)
		handler.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		handler.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	if *sslCert != "" || *sslKey != "" {
		log.Printf("serving HTTPS on %s", *listen)
		err = http.ListenAndServeTLS(*listen, *sslCert, *sslKey, handler)
	} else {
		log.Printf("serving HTTP on %s", *listen)
		err = http.ListenAndServe(*listen, handler)
	}
	log.Printf("ListenAndServe: %v", err)
}
