package mqttpubsub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/brocaar/loraserver/models"
	"github.com/brocaar/lorawan"
	"github.com/eclipse/paho.mqtt.golang"
	. "github.com/smartystreets/goconvey/convey"
)

func TestBackend(t *testing.T) {
	conf := getConfig()

	Convey("Given an MQTT client", t, func() {
		opts := mqtt.NewClientOptions().AddBroker(conf.Server).SetUsername(conf.Username).SetPassword(conf.Password)
		c := mqtt.NewClient(opts)
		token := c.Connect()
		token.Wait()
		So(token.Error(), ShouldBeNil)
		defer c.Disconnect(0)

		Convey("Given a new Backend", func() {
			backend, err := NewBackend(conf.Server, conf.Username, conf.Password)
			So(err, ShouldBeNil)
			defer backend.Close()

			Convey("Given the MQTT client is subscribed to RX packets", func() {
				rxPacketChan := make(chan models.RXPacket)
				token := c.Subscribe("gateway/+/rx", 0, func(c mqtt.Client, msg mqtt.Message) {
					var rxPacket models.RXPacket
					if err := json.Unmarshal(msg.Payload(), &rxPacket); err != nil {
						t.Fatal(err)
					}
					rxPacketChan <- rxPacket
				})
				token.Wait()
				So(token.Error(), ShouldBeNil)

				Convey("When publishing a RXPacket", func() {
					rxPacket := models.RXPacket{
						RXInfo: models.RXInfo{
							MAC:  [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
							Time: time.Now().UTC(),
						},
						PHYPayload: lorawan.PHYPayload{
							MHDR: lorawan.MHDR{
								MType: lorawan.UnconfirmedDataUp,
								Major: lorawan.LoRaWANR1,
							},
							MACPayload: &lorawan.MACPayload{},
						},
					}

					err := backend.PublishGatewayRX([8]byte{1, 2, 3, 4, 5, 6, 7, 8}, rxPacket)
					So(err, ShouldBeNil)

					Convey("Then the same RXPackge was consumed from MQTT", func() {
						packet := <-rxPacketChan
						So(packet, ShouldResemble, rxPacket)
					})
				})
			})

			Convey("Given the backend is subscribed to a gateway MAC", func() {
				err := backend.SubscribeGatewayTX([8]byte{1, 2, 3, 4, 5, 6, 7, 8})
				So(err, ShouldBeNil)

				Convey("When publishing a TXPacket from the MQTT client", func() {
					txPacket := models.TXPacket{
						TXInfo: models.TXInfo{
							MAC: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
						},
						PHYPayload: lorawan.PHYPayload{
							MHDR: lorawan.MHDR{
								MType: lorawan.UnconfirmedDataUp,
								Major: lorawan.LoRaWANR1,
							},
							MACPayload: &lorawan.MACPayload{},
						},
					}
					b, err := json.Marshal(txPacket)
					So(err, ShouldBeNil)
					token := c.Publish("gateway/0102030405060708/tx", 0, false, b)
					token.Wait()
					So(token.Error(), ShouldBeNil)

					Convey("Then the packet is consumed by the backend", func() {
						p := <-backend.TXPacketChan()
						So(p, ShouldResemble, txPacket)
					})
				})
			})
		})
	})
}
