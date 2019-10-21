// Copyright 2019 Cloudbase Solutions SRL
//
//    Licensed under the Apache License, Version 2.0 (the "License"); you may
//    not use this file except in compliance with the License. You may obtain
//    a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
//    WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
//    License for the specific language governing permissions and limitations
//    under the License.

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/gabriel-samfira/coriolis-logger/apiserver"
	"github.com/gabriel-samfira/coriolis-logger/writers/stdout"
	"github.com/gabriel-samfira/coriolis-logger/writers/websocket"

	"github.com/gabriel-samfira/coriolis-logger/config"
	"github.com/gabriel-samfira/coriolis-logger/datastore"
	"github.com/gabriel-samfira/coriolis-logger/logging"
	"github.com/gabriel-samfira/coriolis-logger/syslog"
	"github.com/juju/loggo"
)

var log = loggo.GetLogger("coriolis.logger.cmd")

func main() {
	stop := make(chan os.Signal)
	signal.Notify(stop, syscall.SIGTERM)
	signal.Notify(stop, syscall.SIGINT)
	log.SetLogLevel(loggo.DEBUG)

	cfgFile := flag.String("config", "", "coriolis-logger config file")
	flag.Parse()

	if *cfgFile == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}
	cfg, err := config.NewConfig(*cfgFile)
	if err != nil {
		log.Errorf("error validating config: %q", err)
		os.Exit(1)
	}

	if err := cfg.Validate(); err != nil {
		log.Errorf("failed to validate config: %q", err)
		os.Exit(1)
	}
	// ctx, cancel := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	errChan := make(chan error)

	configuredWriters := []logging.Writer{}

	datastore, err := datastore.GetDatastore(ctx, cfg.Syslog)
	if err != nil {
		log.Errorf("error getting datastore: %q", err)
		os.Exit(1)
	}
	if err := datastore.Start(); err != nil {
		log.Errorf("error starting datastore: %q", err)
		os.Exit(1)
	}
	configuredWriters = append(configuredWriters, datastore)

	if cfg.Syslog.LogToStdout {
		stdoutWriter, err := stdout.NewStdOutWriter()
		if err != nil {
			log.Errorf("error getting stdout datastore: %q", err)
			os.Exit(1)
		}
		configuredWriters = append(configuredWriters, stdoutWriter)
	}

	websocketWorker := websocket.NewHub(ctx)
	if err := websocketWorker.Start(); err != nil {
		log.Errorf("error starting websocket worker: %q", err)
		os.Exit(1)
	}
	configuredWriters = append(configuredWriters, websocketWorker)

	writer := logging.NewAggregateWriter(configuredWriters...)

	syslogSvc, err := syslog.NewSyslogServer(ctx, cfg.Syslog, writer, errChan)
	if err != nil {
		log.Errorf("error getting syslog worker: %q", err)
		os.Exit(1)
	}
	if err := syslogSvc.Start(); err != nil {
		log.Errorf("error starting syslog worker: %q", err)
		os.Exit(1)
	}

	apiServer, err := apiserver.GetAPIServer(
		cfg.APIServer, websocketWorker, datastore)
	if err != nil {
		log.Errorf("error getting api worker: %q", err)
		os.Exit(1)
	}

	if err := apiServer.Start(); err != nil {
		log.Errorf("error starting api worker: %q", err)
		os.Exit(1)
	}

	select {
	case <-stop:
		log.Infof("shutting down gracefully")
		// if err := syslogSvc.Stop(); err != nil {
		// 	log.Errorf("error stopping syslog worker: %q", err)
		// }
		cancel()
	case err := <-errChan:
		log.Errorf("worker set error: %q. Shutting down", err)
		// if err := syslogSvc.Stop(); err != nil {
		// 	log.Errorf("error stopping syslog worker: %q", err)
		// }
		cancel()
	}
	syslogSvc.Wait()
	datastore.Wait()
	apiServer.Stop()
}
