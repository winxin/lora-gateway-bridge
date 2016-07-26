package thethingsnetwork

import (
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"

	pb_gateway "github.com/TheThingsNetwork/ttn/api/gateway"
	pb_protocol "github.com/TheThingsNetwork/ttn/api/protocol"
	pb_lorawan "github.com/TheThingsNetwork/ttn/api/protocol/lorawan"
	pb "github.com/TheThingsNetwork/ttn/api/router"
	"github.com/brocaar/loraserver/models"
	"github.com/brocaar/lorawan"
	"google.golang.org/grpc"

	. "github.com/smartystreets/goconvey/convey"
)

func TestBackend(t *testing.T) {
	Convey("Given a mocked TTN Router", t, func() {

		rand.Seed(time.Now().UnixNano())
		port := rand.Intn(500) + 7000

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			panic(err)
		}
		defer lis.Close()

		rtr := newMockRouter()
		s := grpc.NewServer()
		defer s.Stop()

		pb.RegisterRouterServer(s, rtr)
		go s.Serve(lis)

		Convey("Given a New Backend", func() {
			backend, err := NewBackend("", fmt.Sprintf("localhost:%d", port), "super-secure-token")
			So(err, ShouldBeNil)
			defer backend.Close()

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

				Convey("Then the RXPacket was received by the TTN Router", func() {
					select {
					case <-time.After(1 * time.Second):
						panic("Did not receive")
					case packet := <-rtr.uplink:
						So(packet, ShouldNotBeNil)
					}
				})
			})

			Convey("When publishing a GatewayStatsPacket", func() {
				statsPacket := models.GatewayStatsPacket{}

				err := backend.PublishGatewayStats([8]byte{1, 2, 3, 4, 5, 6, 7, 8}, statsPacket)
				So(err, ShouldBeNil)

				Convey("Then the GatewayStatsPacket was received by the TTN Router", func() {
					select {
					case <-time.After(1 * time.Second):
						panic("Did not receive")
					case packet := <-rtr.gatewayStatus:
						So(packet, ShouldNotBeNil)
					}
				})
			})

			Convey("Given the backend is subscribed to a gateway MAC", func() {
				err := backend.SubscribeGatewayTX([8]byte{1, 2, 3, 4, 5, 6, 7, 8})
				So(err, ShouldBeNil)

				Convey("When publishing a downlink message from the Router", func() {
					rtr.downlink <- &pb.DownlinkMessage{
						Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8},
						ProtocolConfiguration: &pb_protocol.TxConfiguration{Protocol: &pb_protocol.TxConfiguration_Lorawan{Lorawan: &pb_lorawan.TxConfiguration{
							DataRate: "SF7BW125",
						}}},
						GatewayConfiguration: &pb_gateway.TxConfiguration{},
					}

					Convey("Then the packet is consumed by the backend", func() {
						select {
						case <-time.After(1 * time.Second):
							panic("Did not receive")
						case packet := <-backend.TXPacketChan():
							So(packet, ShouldNotBeNil)
						}
					})
				})

				Convey("When unsubscribing from the gateway MAC", func() {
					err := backend.UnSubscribeGatewayTX([8]byte{1, 2, 3, 4, 5, 6, 7, 8})
					So(err, ShouldBeNil)
				})

			})

		})

	})
}
