// Copyright (c) 2018-2019, AT&T Intellectual Property. All rights reserved.
// SPDX-License-Identifier: GPL-2.0-only
package forwarding

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/danos/vci-service-dns/internal/fswatcher"
	"github.com/danos/vci-service-dns/internal/log"
	"github.com/danos/vci-service-dns/internal/process"
	"github.com/msoap/byline"
)

const dhcpFile = `### Autogenerated by vci-service-dns
### Note: Manual changes to this file will be lost.
{{$interface := .Interface -}}
{{range .Nameservers -}}
server={{.}}	# dhcp {{$interface}}
{{end -}}
`

var dhcpFileTemplate *template.Template

func init() {
	t := template.New("DhcpConf")
	t.Funcs(template.FuncMap{})
	dhcpFileTemplate = template.Must(t.Parse(dhcpFile))
}

type dhcpConfig struct {
	interfaces  []string
	proc        process.Process
	watcher     *dhcpWatcher
	watchFmt    string
	confFileFmt string
}

func (c *dhcpConfig) removeConfFiles() {
	for _, intf := range c.interfaces {
		err := os.Remove(fmt.Sprintf(c.confFileFmt, intf))
		if err != nil {
			log.Dlog.Println("dhcp-config:", err)
		}
	}
}

func (c *dhcpConfig) Set(new []string) error {
	if len(new) == 0 {
		c.watcher.stop()
		c.removeConfFiles()
		c.watcher = nil
	} else {
		c.watcher.stop()
		c.removeConfFiles()
		c.watcher = startDhcpWatcher(new, c.proc, c.watchFmt, c.confFileFmt)
	}
	c.interfaces = new
	return nil
}

// This would be better done by listening for a notification on VCI.
// TODO: implement this notification in dhclient script and switch this
//       to use it.
type dhcpWatcher struct {
	proc        process.Process
	watcher     *fswatcher.Watcher
	fileToIntf  map[string]string
	confFileFmt string
}

func startDhcpWatcher(
	interfaces []string,
	proc process.Process,
	watchPattern, confFileFmt string,
) *dhcpWatcher {
	out := &dhcpWatcher{
		proc:        proc,
		fileToIntf:  make(map[string]string),
		confFileFmt: confFileFmt,
	}
	opts := make([]fswatcher.WatcherOpt, 0, len(interfaces)+2)
	opts = append(opts,
		fswatcher.LogPrefix("dhcp nameserver watcher:"),
		fswatcher.Logger(log.Dlog),
	)
	for _, intf := range interfaces {
		file := fmt.Sprintf(watchPattern, intf)
		out.fileToIntf[file] = intf
		opts = append(opts,
			fswatcher.Handler(filepath.Dir(file), nil),
			fswatcher.Handler(file, out),
		)
	}
	for file := range out.fileToIntf {
		if _, err := os.Stat(file); err == nil {
			err := out.writeConffileFromDhclient(file)
			if err != nil {
				log.Dlog.Println("dhcp nameserver watcher:", err)
			}
		}
	}
	out.watcher = fswatcher.Start(opts...)
	return out
}

func (w *dhcpWatcher) writeConffileFromDhclient(dhclientFile string) error {
	f, err := os.Open(dhclientFile)
	if err != nil {
		return err
	}
	defer f.Close()

	intf := w.fileToIntf[dhclientFile]
	out, err := os.OpenFile(fmt.Sprintf(w.confFileFmt, intf), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	err = writeDnsmasqDhcpConfig(out, intf, readDhcpNameservers(f))
	if err != nil {
		return err
	}

	return nil
}

func (w *dhcpWatcher) createOrWrite(name string) error {
	err := w.writeConffileFromDhclient(name)
	if err != nil {
		return err
	}
	return w.proc.Reload()
}

func (w *dhcpWatcher) Write(name string) error {
	return w.createOrWrite(name)
}

func (w *dhcpWatcher) Create(name string) error {
	return w.createOrWrite(name)
}

func (w *dhcpWatcher) stop() {
	if w == nil {
		return
	}
	w.watcher.Stop()
}

func writeDnsmasqDhcpConfig(w io.Writer, intf string, ns []string) error {
	conf := struct {
		Interface   string
		Nameservers []string
	}{
		Interface:   intf,
		Nameservers: ns,
	}
	if len(ns) == 0 {
		return nil
	}
	return dhcpFileTemplate.Execute(w, &conf)
}

func readDhcpNameservers(r io.Reader) []string {
	var ns []string
	err := byline.NewReader(r).
		GrepByRegexp(regexp.MustCompile("new_domain_name_servers")).
		SetFS(regexp.MustCompile("[= ]")).
		AWKMode(func(line string, fields []string, vars byline.AWKVars) (string, error) {
			for _, server := range fields[1:] {
				ns = append(ns, strings.Trim(server, "'"))
			}
			return "", nil
		}).
		Discard()
	if err != nil {
		log.Wlog.Println(err)
	}
	return ns
}
