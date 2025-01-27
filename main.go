package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"github.com/Glimesh/waveguide/internal/inputs/fs"
	"github.com/Glimesh/waveguide/internal/inputs/ftl"
	"github.com/Glimesh/waveguide/internal/inputs/janus"
	"github.com/Glimesh/waveguide/internal/inputs/rtmp"
	"github.com/Glimesh/waveguide/internal/inputs/whip"
	"github.com/Glimesh/waveguide/internal/outputs/hls"
	"github.com/Glimesh/waveguide/internal/outputs/whep"
	"github.com/Glimesh/waveguide/pkg/control"
	"github.com/Glimesh/waveguide/pkg/orchestrators/dummy_orchestrator"
	"github.com/Glimesh/waveguide/pkg/orchestrators/rt_orchestrator"
	"github.com/Glimesh/waveguide/pkg/services/dummy_service"
	"github.com/Glimesh/waveguide/pkg/services/glimesh"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

func main() {
	log := logrus.New()

	hostname, err := os.Hostname()
	if err != nil {
		// How tf
		log.Fatal(err)
	}
	log.Debugf("Server Hostname: %s", hostname)

	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(".")
	viper.SetDefault("control.log_level", "info")
	err = viper.ReadInConfig()
	if err != nil {
		log.Fatal(fmt.Errorf("fatal error config file: %w", err))
	}

	// Temporary for debugging
	go func() {
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	level, err := logrus.ParseLevel(viper.GetString("control.log_level"))
	if err != nil {
		log.Fatal(fmt.Errorf("fatal error config file: %w", err))
	}
	log.SetLevel(level)

	var service control.Service
	switch viper.GetString("control.service") {
	case "dummy":
		service = dummy_service.New(dummy_service.Config{})
	case "glimesh":
		var glimeshConfig glimesh.Config
		unmarshalConfig("service.glimesh", &glimeshConfig)
		service = glimesh.New(glimeshConfig)
	}
	service.SetLogger(log.WithFields(logrus.Fields{
		"service": service.Name(),
	}))
	service.Connect()

	var orchestrator control.Orchestrator
	switch viper.GetString("control.orchestrator") {
	case "dummy":
		orchestrator = dummy_orchestrator.New(dummy_orchestrator.Config{}, hostname)
	case "rt":
		var rtConfig rt_orchestrator.Config
		unmarshalConfig("orchestrator.rtrouter", &rtConfig)
		orchestrator = rt_orchestrator.New(rtConfig, hostname)
	}
	orchestrator.SetLogger(log.WithFields(logrus.Fields{
		"orchestrator": orchestrator.Name(),
	}))
	orchestrator.Connect()

	var controlConfig control.Config
	unmarshalConfig("control", &controlConfig)
	controlConfig.Hostname = hostname
	ctrl := control.New(controlConfig)
	ctrl.SetService(service)
	ctrl.SetOrchestrator(orchestrator)
	ctrl.SetLogger(log.WithFields(logrus.Fields{
		"control": "waveguide",
	}))

	ctx := context.Background()
	for inputName := range viper.GetStringMap("input") {
		inputType := viper.GetString(fmt.Sprintf("input.%s.type", inputName))
		configKey := fmt.Sprintf("input.%s", inputName)

		var input control.Input

		switch inputType {
		case "fs":
			var fsConfig fs.FSSourceConfig
			unmarshalConfig(configKey, &fsConfig)
			input = fs.New(fsConfig)
		case "janus":
			var janusConfig janus.JanusSourceConfig
			unmarshalConfig(configKey, &janusConfig)
			input = janus.New(janusConfig)
		case "rtmp":
			var rtmpConfig rtmp.RTMPSourceConfig
			unmarshalConfig(configKey, &rtmpConfig)
			input = rtmp.New(rtmpConfig)
		case "ftl":
			var ftlConfig ftl.FTLSourceConfig
			unmarshalConfig(configKey, &ftlConfig)
			input = ftl.New(ftlConfig)
		case "whip":
			var whipConfig whip.WHIPSourceConfig
			unmarshalConfig(configKey, whipConfig)
			input = whip.New(whipConfig)
		default:
			log.Fatalf("could not find input type %s", inputType)
		}
		input.SetControl(ctrl)
		input.SetLogger(log.WithFields(logrus.Fields{"input": inputType}))
		go input.Listen(ctx)
	}

	for outputName := range viper.GetStringMap("output") {
		outputType := viper.Get(fmt.Sprintf("output.%s.type", outputName))
		configKey := fmt.Sprintf("output.%s", outputName)

		var output control.Output

		switch outputType {
		case "hls":
			var hlsConfig hls.HLSConfig
			unmarshalConfig(configKey, &hlsConfig)
			output = hls.New(hlsConfig)
		case "whep":
			var whepConfig whep.WHEPConfig
			unmarshalConfig(configKey, &whepConfig)
			output = whep.New(whepConfig)
		}

		output.SetControl(ctrl)
		output.SetLogger(log.WithFields(logrus.Fields{"output": outputName}))
		go output.Listen(ctx)
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Info("Exiting Waveguide and cleaning up")
		ctrl.Shutdown()
		os.Exit(0)
	}()

	ctrl.StartHTTPServer()
}

func unmarshalConfig(configKey string, config interface{}) {
	err := viper.UnmarshalKey(configKey, &config)
	if err != nil {
		panic(err)
	}
}
