// Copyright (c) 2018-2019, AT&T Intellectual Property. All rights reserved.
// SPDX-License-Identifier: GPL-2.0-only
package dynamic

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/danos/vci-service-dns/internal/log"
	"github.com/danos/vci-service-dns/internal/process"
)

const (
	ddclientUnitFmt  = "ddclient@%s.service"
	ddclientConfFmt  = "ddclient_%s.conf"
	ddclientPidFmt   = "ddclient_%s.pid"
	ddclientCacheFmt = "ddclient_%s.cache"
	ddclientEnvFile  = "ddclient.env"
)

const cfgFile = `#
# autogenerated by vci-service-dns on {{Date}}
#
daemon=1m
syslog=yes
ssl=yes
pid={{.PidFile}}
cache={{.CacheFile}}
use=if, if={{.Conf.Name}}


{{range .Conf.Service -}}
{{$service := . -}}
{{range .HostName -}}
protocol={{MapServiceName $service.Name}}
{{if ne $service.Server "" -}}
server={{$service.Server}},
{{end -}}
max-interval=28d
login={{$service.Login}}
password={{$service.Password}}
{{.}}

{{end -}}
{{end -}}
`
const envFile = `#
# autogenerated by vci-service-dns
#
DDCLIENT_VRF_NAME={{.VRFName}}
DDCLIENT_IF_CONF={{.ConfFile}}
`

var cfgFileTemplate *template.Template
var envFileTemplate *template.Template

func init() {
	t := template.New("DynamicConf")
	t.Funcs(template.FuncMap{
		"MapServiceName": mapServiceNames,
		"Date": func() string {
			return time.Now().Format(time.UnixDate)
		},
	})
	cfgFileTemplate = template.Must(t.Parse(cfgFile))
	t = template.New("DynamicEnv")
	t.Funcs(template.FuncMap{})
	envFileTemplate = template.Must(t.Parse(envFile))
}

type ConfigData struct {
	Interface []InterfaceConfigData `rfc7951:"interface"`
}

type InterfaceConfigData struct {
	Name    string              `rfc7951:"tagnode"`
	Service []ServiceConfigData `rfc7951:"service"`
}

type ServiceConfigData struct {
	Name     string   `rfc7951:"tagnode"`
	Password string   `rfc7951:"password"`
	Login    string   `rfc7951:"login"`
	Server   string   `rfc7951:"server"`
	HostName []string `rfc7951:"host-name"`
}

type ConfigOpt func(*Config)

func DDClientRunDir(dir string) ConfigOpt {
	return func(c *Config) {
		c.ddclientRunDir = dir
	}
}

func DDClientCacheDir(dir string) ConfigOpt {
	return func(c *Config) {
		c.ddclientCacheDir = dir
	}
}

func DDClientConfigDir(dir string) ConfigOpt {
	return func(c *Config) {
		c.ddclientConfigDir = dir
	}
}

func DDClientEnvDirFmt(fmt string) ConfigOpt {
	return func(c *Config) {
		c.ddclientEnvDirFmt = fmt
	}
}

func VRFHelpers(sub process.VRFSubscriber, chk process.VRFChecker) ConfigOpt {
	return func(c *Config) {
		c.vrfSub = sub
		c.vrfChk = chk
		origPcons := c.pCons
		c.pCons = func(name string) process.Process {
			if c.vrfSub != nil {
				return process.NewVrfDependantProcess(
					c.instanceName,
					c.vrfSub,
					c.vrfChk,
					origPcons(name))
			}
			return origPcons(name)
		}
	}
}

type Config struct {
	currentConfig     atomic.Value
	runningInterfaces atomic.Value

	instanceName string
	// options
	ddclientRunDir    string
	ddclientCacheDir  string
	ddclientConfigDir string
	ddclientEnvDirFmt string

	pCons func(string) process.Process

	vrfSub process.VRFSubscriber
	vrfChk process.VRFChecker
}

func NewInstanceConfig(name string, opts ...ConfigOpt) *Config {
	conf := NewConfig(opts...)
	conf.instanceName = name
	return conf
}

func NewConfig(opts ...ConfigOpt) *Config {
	const (
		ddclientRunDir    = "/var/run/ddclient"
		ddclientCacheDir  = "/var/cache/ddclient"
		ddclientConfigDir = "/etc/ddclient"
		ddclientEnvDirFmt = "/run/dns/%s"
	)
	conf := &Config{
		instanceName:      "default",
		ddclientRunDir:    ddclientRunDir,
		ddclientCacheDir:  ddclientCacheDir,
		ddclientConfigDir: ddclientConfigDir,
		ddclientEnvDirFmt: ddclientEnvDirFmt,
		pCons:             process.NewSystemdProcess,
	}
	conf.currentConfig.Store(&ConfigData{})
	conf.runningInterfaces.Store(make(map[string]process.Process))
	for _, opt := range opts {
		opt(conf)
	}
	return conf
}

func (c *Config) Get() *ConfigData {
	return c.currentConfig.Load().(*ConfigData)
}

func (c *Config) Set(new *ConfigData) error {
	const logPrefix = "dns-dynamic-config-set"
	old := c.Get()

	newInterfaces := make(map[string]InterfaceConfigData)
	if new != nil {
		for _, intf := range new.Interface {
			newInterfaces[intf.Name] = intf
		}
	}

	oldInterfaces := make(map[string]InterfaceConfigData)
	for _, intf := range old.Interface {
		oldInterfaces[intf.Name] = intf
	}

	c.stopInactiveInterfaces(newInterfaces)

	c.ensureEnvironment()

	c.updateActiveInterfaces(oldInterfaces, newInterfaces)

	if new != nil {
		c.currentConfig.Store(new)
	} else {
		c.cleanupEnvironment()
		c.currentConfig.Store(&ConfigData{})
	}

	return nil
}

func (c *Config) stopInactiveInterfaces(new map[string]InterfaceConfigData) {
	stopInterfaces := c.getInactiveInterfaces(new)
	for intf, proc := range stopInterfaces {
		c.stopInterface(intf, proc)
	}
}

func (c *Config) getInactiveInterfaces(new map[string]InterfaceConfigData) map[string]process.Process {
	old := c.Get()
	runningIntf := c.runningInterfaces.Load().(map[string]process.Process)
	out := make(map[string]process.Process)
	for _, intf := range old.Interface {
		_, ok := new[intf.Name]
		if ok {
			continue
		}
		out[intf.Name] = runningIntf[intf.Name]
	}
	return out
}

func (c *Config) stopInterface(intf string, proc process.Process) {
	var logPrefix = "dns-dynamic-config-set stop-interface " + intf + " "
	err := proc.Stop()
	if err != nil {
		log.Dlog.Println(logPrefix, "stop process", err)
	}

	confFile := fmt.Sprintf("%s/"+ddclientConfFmt,
		c.ddclientConfigDir, intf)
	pidFile := fmt.Sprintf("%s/"+ddclientPidFmt,
		c.ddclientRunDir, intf)
	cacheFile := fmt.Sprintf("%s/"+ddclientCacheFmt,
		c.ddclientCacheDir, intf)
	envFile := fmt.Sprintf(c.ddclientEnvDirFmt+"/%s",
		intf, ddclientEnvFile)
	files := []string{confFile, pidFile, cacheFile, envFile}
	for _, file := range files {
		err = os.Remove(file)
		if err != nil {
			log.Dlog.Println(logPrefix, err)
		}
	}
}

func (c *Config) ensureEnvironment() {
	const logPrefix = "dns-dynamic-config-set ensure-environment"
	dirs := []string{
		c.ddclientConfigDir,
		c.ddclientRunDir,
		c.ddclientCacheDir,
	}
	for _, dir := range dirs {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			log.Dlog.Println(logPrefix, err)
		}
	}

}

func (c *Config) updateActiveInterfaces(old, new map[string]InterfaceConfigData) {
	knownProcs := c.runningInterfaces.Load().(map[string]process.Process)
	newProcs := make(map[string]process.Process)
	for _, intf := range new {
		proc, ok := knownProcs[intf.Name]
		if !ok {
			proc = c.pCons(fmt.Sprintf(ddclientUnitFmt, intf.Name))
		}
		newProcs[intf.Name] = proc

		if reflect.DeepEqual(intf, old[intf.Name]) {
			continue
		}

		c.updateInterface(&intf, proc)
	}
	c.runningInterfaces.Store(newProcs)
}

func (c *Config) updateInterface(intf *InterfaceConfigData, proc process.Process) {
	const logPrefix = "dns-dynamic-config-set update-interface"
	confFile := fmt.Sprintf("%s/"+ddclientConfFmt,
		c.ddclientConfigDir, intf.Name)
	pidFile := fmt.Sprintf("%s/"+ddclientPidFmt,
		c.ddclientRunDir, intf.Name)
	cacheFile := fmt.Sprintf("%s/"+ddclientCacheFmt,
		c.ddclientCacheDir, intf.Name)
	envFile := fmt.Sprintf(c.ddclientEnvDirFmt+"/%s",
		intf.Name, ddclientEnvFile)

	f, err := os.OpenFile(confFile,
		os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Elog.Println(logPrefix, err)
		return
	}
	err = writeConfig(f, cacheFile, pidFile, intf)
	if err != nil {
		log.Elog.Println(logPrefix, err)
	}
	f.Close()

	os.MkdirAll(filepath.Dir(envFile), 0755)
	envf, err := os.OpenFile(envFile,
		os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Elog.Println(logPrefix, err)
		return
	}
	err = writeEnvFile(envf, c.instanceName, confFile)
	if err != nil {
		log.Elog.Println(logPrefix, err)
	}
	envf.Close()

	err = proc.Reload()
	if err != nil {
		log.Elog.Println(logPrefix, err)
	}
}

func (c *Config) cleanupEnvironment() {
	const logPrefix = "dns-dynamic-config-set cleanup"
	removeDirs := []string{c.ddclientRunDir, c.ddclientCacheDir}
	for _, dir := range removeDirs {
		err := os.RemoveAll(dir)
		if err != nil {
			log.Dlog.Println(logPrefix, err)
		}
	}
}

func writeConfig(
	w io.Writer,
	cacheFile string,
	pidFile string,
	c *InterfaceConfigData,
) error {
	tmplInput := struct {
		PidFile   string
		CacheFile string
		Conf      *InterfaceConfigData
	}{
		PidFile:   pidFile,
		CacheFile: cacheFile,
		Conf:      c,
	}
	return cfgFileTemplate.Execute(w, &tmplInput)
}

func writeEnvFile(w io.Writer, instanceName, confFile string) error {
	tmplInput := struct {
		VRFName  string
		ConfFile string
	}{
		VRFName:  instanceName,
		ConfFile: confFile,
	}
	return envFileTemplate.Execute(w, &tmplInput)
}

func mapServiceNames(in string) string {
	switch in {
	case "dslreports":
		return "dslreports1"
	case "dyndns":
		return "dyndns2"
	case "zoneedit":
		return "zoneedit1"
	default:
		return in
	}
}
