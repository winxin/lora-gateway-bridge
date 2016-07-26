package thethingsnetwork

import (
	"errors"
	"io"

	"golang.org/x/net/context"

	"github.com/TheThingsNetwork/ttn/api"
	pb_gateway "github.com/TheThingsNetwork/ttn/api/gateway"
	pb "github.com/TheThingsNetwork/ttn/api/router"
)

type mockRouter struct {
	gatewayStatus      chan *pb_gateway.Status
	uplink             chan *pb.UplinkMessage
	downlink           chan *pb.DownlinkMessage
	activationRequest  chan *pb.DeviceActivationRequest
	activationResponse chan *pb.DeviceActivationResponse
}

func newMockRouter() *mockRouter {
	return &mockRouter{
		gatewayStatus:      make(chan *pb_gateway.Status, 10),
		uplink:             make(chan *pb.UplinkMessage, 10),
		downlink:           make(chan *pb.DownlinkMessage, 10),
		activationRequest:  make(chan *pb.DeviceActivationRequest, 10),
		activationResponse: make(chan *pb.DeviceActivationResponse, 10),
	}
}

func (r *mockRouter) GatewayStatus(stream pb.Router_GatewayStatusServer) error {
	for {
		status, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&api.Ack{})
		}
		if err != nil {
			return err
		}
		r.gatewayStatus <- status
		return nil
	}
}

func (r *mockRouter) Uplink(stream pb.Router_UplinkServer) error {
	for {
		uplink, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&api.Ack{})
		}
		if err != nil {
			return err
		}
		r.uplink <- uplink
		return nil
	}
}

// Subscribe implements RouterServer interface (github.com/TheThingsNetwork/ttn/api/router)
func (r *mockRouter) Subscribe(req *pb.SubscribeRequest, stream pb.Router_SubscribeServer) error {
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case downlink := <-r.downlink:
			if err := stream.Send(downlink); err != nil {
				return err
			}
		}
	}
}

// Activate implements RouterServer interface (github.com/TheThingsNetwork/ttn/api/router)
func (r *mockRouter) Activate(ctx context.Context, req *pb.DeviceActivationRequest) (*pb.DeviceActivationResponse, error) {
	r.activationRequest <- req
	res := <-r.activationResponse
	if res == nil {
		return nil, errors.New("No response")
	}
	return res, nil
}
