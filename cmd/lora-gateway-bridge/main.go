package main

//go:generate ./doc.sh

import (
	"os"
	"os/signal"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/brocaar/lora-gateway-bridge/backend/thethingsnetwork"
	"github.com/brocaar/lora-gateway-bridge/gateway"
	"github.com/brocaar/lorawan"
	"github.com/codegangsta/cli"
)

var version string // set by the compiler

func run(c *cli.Context) error {
	log.SetLevel(log.Level(uint8(c.Int("log-level"))))

	ttn, err := thethingsnetwork.NewBackend(
		c.String("ttn-discovery-server"),
		c.String("ttn-router"),
		"token",
	)
	if err != nil {
		log.Fatalf("could not setup ttn backend: %s", err)
	}
	defer ttn.Close()

	onNew := func(mac lorawan.EUI64) error {
		return ttn.SubscribeGatewayTX(mac)
	}

	onDelete := func(mac lorawan.EUI64) error {
		return ttn.UnSubscribeGatewayTX(mac)
	}

	gw, err := gateway.NewBackend(c.String("udp-bind"), onNew, onDelete)
	if err != nil {
		log.Fatalf("could not setup gateway backend: %s", err)
	}
	defer gw.Close()

	go func() {
		for rxPacket := range gw.RXPacketChan() {
			if err := ttn.PublishGatewayRX(rxPacket.RXInfo.MAC, rxPacket); err != nil {
				log.Errorf("could not publish RXPacket: %s", err)
			}
		}
	}()

	go func() {
		for stats := range gw.StatsChan() {
			if err := ttn.PublishGatewayStats(stats.MAC, stats); err != nil {
				log.Errorf("could not publish GatewayStatsPacket: %s", err)
			}
		}
	}()

	go func() {
		for txPacket := range ttn.TXPacketChan() {
			if err := gw.Send(txPacket); err != nil {
				log.Errorf("could not send TXPacket: %s", err)
			}
		}
	}()

	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	log.WithField("signal", <-sigChan).Info("signal received")
	log.Warning("shutting down server")
	return nil
}

func main() {
	app := cli.NewApp()
	app.Name = "lora-gateway-bridge"
	app.Usage = "abstracts the packet_forwarder protocol into JSON over MQTT"
	app.Copyright = "See http://github.com/brocaar/lora-gateway-bridge for copyright information"
	app.Version = version
	app.Action = run
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "udp-bind",
			Usage:  "ip:port to bind the UDP listener to",
			Value:  "0.0.0.0:1700",
			EnvVar: "UDP_BIND",
		},
		cli.StringFlag{
			Name:   "ttn-discovery-server",
			Usage:  "TTN Discovery Server",
			Value:  "discover.thethingsnetwork.org:1900",
			EnvVar: "TTN_DISCOVERY_SERVER",
		},
		cli.StringFlag{
			Name:   "ttn-router",
			Usage:  "TTN Router ID",
			Value:  "dev",
			EnvVar: "TTN_ROUTER",
		},
		cli.IntFlag{
			Name:   "log-level",
			Value:  4,
			Usage:  "debug=5, info=4, warning=3, error=2, fatal=1, panic=0",
			EnvVar: "LOG_LEVEL",
		},
	}
	app.Run(os.Args)
}
