package gateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/brocaar/loraserver/models"
	"github.com/brocaar/lorawan"
	"github.com/brocaar/lorawan/band"

	log "github.com/Sirupsen/logrus"
)

var errGatewayDoesNotExist = errors.New("gateway does not exist")
var gatewayCleanupDuration = -1 * time.Minute
var loRaDataRateRegex = regexp.MustCompile(`SF(\d+)BW(\d+)`)

type udpPacket struct {
	addr *net.UDPAddr
	data []byte
}

type gateway struct {
	addr            *net.UDPAddr
	protocolVersion ProtocolVersion
	lastSeen        time.Time
}

type gateways struct {
	sync.RWMutex
	gateways map[lorawan.EUI64]gateway
	onNew    func(lorawan.EUI64) error
	onDelete func(lorawan.EUI64) error
}

func (c *gateways) get(mac lorawan.EUI64) (gateway, error) {
	defer c.RUnlock()
	c.RLock()
	gw, ok := c.gateways[mac]
	if !ok {
		return gw, errGatewayDoesNotExist
	}
	return gw, nil
}

func (c *gateways) set(mac lorawan.EUI64, gw gateway) error {
	defer c.Unlock()
	c.Lock()
	_, ok := c.gateways[mac]
	if !ok && c.onNew != nil {
		if err := c.onNew(mac); err != nil {
			return err
		}
	}
	c.gateways[mac] = gw
	return nil
}

func (c *gateways) cleanup() error {
	defer c.Unlock()
	c.Lock()
	for mac := range c.gateways {
		if c.gateways[mac].lastSeen.Before(time.Now().Add(gatewayCleanupDuration)) {
			if c.onDelete != nil {
				if err := c.onDelete(mac); err != nil {
					return err
				}
			}
			delete(c.gateways, mac)
		}
	}
	return nil
}

// Backend implements a Semtech gateway backend.
type Backend struct {
	conn        *net.UDPConn
	rxChan      chan models.RXPacket
	statsChan   chan models.GatewayStatsPacket
	udpSendChan chan udpPacket
	closed      bool
	gateways    gateways
	wg          sync.WaitGroup
}

// NewBackend creates a new backend.
func NewBackend(bind string, onNew func(lorawan.EUI64) error, onDelete func(lorawan.EUI64) error) (*Backend, error) {
	addr, err := net.ResolveUDPAddr("udp", bind)
	if err != nil {
		return nil, err
	}
	log.WithField("addr", addr).Info("gateway: starting gateway udp listener")
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	b := &Backend{
		conn:        conn,
		rxChan:      make(chan models.RXPacket),
		statsChan:   make(chan models.GatewayStatsPacket),
		udpSendChan: make(chan udpPacket),
		gateways: gateways{
			gateways: make(map[lorawan.EUI64]gateway),
			onNew:    onNew,
			onDelete: onDelete,
		},
	}

	go func() {
		for {
			if err := b.gateways.cleanup(); err != nil {
				log.Errorf("gateway: gateways cleanup failed: %s", err)
			}
			time.Sleep(time.Minute)
		}
	}()

	go func() {
		b.wg.Add(1)
		err := b.readPackets()
		if !b.closed {
			log.Fatal(err)
		}
		b.wg.Done()
	}()

	go func() {
		b.wg.Add(1)
		err := b.sendPackets()
		if !b.closed {
			log.Fatal(err)
		}
		b.wg.Done()
	}()

	return b, nil
}

// Close closes the backend.
func (b *Backend) Close() error {
	log.Info("gateway: closing gateway backend")
	b.closed = true
	close(b.udpSendChan)
	if err := b.conn.Close(); err != nil {
		return err
	}
	log.Info("gateway: handling last packets")
	b.wg.Wait()
	return nil
}

// RXPacketChan returns the channel containing the received RX packets.
func (b *Backend) RXPacketChan() chan models.RXPacket {
	return b.rxChan
}

// StatsChan returns the channel containg the received gateway stats.
func (b *Backend) StatsChan() chan models.GatewayStatsPacket {
	return b.statsChan
}

// Send sends the given packet to the gateway.
func (b *Backend) Send(txPacket models.TXPacket) error {
	gw, err := b.gateways.get(txPacket.TXInfo.MAC)
	if err != nil {
		return err
	}
	txpk, err := newTXPKFromTXPacket(txPacket)
	if err != nil {
		return err
	}
	pullResp := PullRespPacket{
		ProtocolVersion: gw.protocolVersion,
		Payload: PullRespPayload{
			TXPK: txpk,
		},
	}
	bytes, err := pullResp.MarshalBinary()
	if err != nil {
		return fmt.Errorf("gateway: json marshall PullRespPacket error: %s", err)
	}
	b.udpSendChan <- udpPacket{
		data: bytes,
		addr: gw.addr,
	}
	return nil
}

func (b *Backend) readPackets() error {
	buf := make([]byte, 65507) // max udp data size
	for {
		i, addr, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			return fmt.Errorf("gateway: read from udp error: %s", err)
		}
		data := make([]byte, i)
		copy(data, buf[:i])
		go func(data []byte) {
			if err := b.handlePacket(addr, data); err != nil {
				log.WithFields(log.Fields{
					"data_base64": base64.StdEncoding.EncodeToString(data),
					"addr":        addr,
				}).Errorf("gateway: could not handle packet: %s", err)
			}
		}(data)
	}
}

func (b *Backend) sendPackets() error {
	for p := range b.udpSendChan {
		pt, err := GetPacketType(p.data)
		if err != nil {
			log.WithFields(log.Fields{
				"addr":        p.addr,
				"data_base64": base64.StdEncoding.EncodeToString(p.data),
			}).Error("gateway: unknown packet type")
			continue
		}
		log.WithFields(log.Fields{
			"addr": p.addr,
			"type": pt,
		}).Info("gateway: sending udp packet to gateway")

		if _, err := b.conn.WriteToUDP(p.data, p.addr); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) handlePacket(addr *net.UDPAddr, data []byte) error {
	pt, err := GetPacketType(data)
	if err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"addr": addr,
		"type": pt,
	}).Info("gateway: received udp packet from gateway")

	switch pt {
	case PushData:
		return b.handlePushData(addr, data)
	case PullData:
		return b.handlePullData(addr, data)
	case TXACK:
		return b.handleTXACK(addr, data)
	default:
		return fmt.Errorf("gateway: unknown packet type: %s", pt)
	}
}

func (b *Backend) handlePullData(addr *net.UDPAddr, data []byte) error {
	var p PullDataPacket
	if err := p.UnmarshalBinary(data); err != nil {
		return err
	}
	ack := PullACKPacket{
		ProtocolVersion: p.ProtocolVersion,
		RandomToken:     p.RandomToken,
	}
	bytes, err := ack.MarshalBinary()
	if err != nil {
		return err
	}

	err = b.gateways.set(p.GatewayMAC, gateway{
		addr:            addr,
		protocolVersion: p.ProtocolVersion,
		lastSeen:        time.Now().UTC(),
	})
	if err != nil {
		return err
	}

	b.udpSendChan <- udpPacket{
		addr: addr,
		data: bytes,
	}
	return nil
}

func (b *Backend) handlePushData(addr *net.UDPAddr, data []byte) error {
	var p PushDataPacket
	if err := p.UnmarshalBinary(data); err != nil {
		return err
	}

	// ack the packet
	ack := PushACKPacket{
		ProtocolVersion: p.ProtocolVersion,
		RandomToken:     p.RandomToken,
	}
	bytes, err := ack.MarshalBinary()
	if err != nil {
		return err
	}
	b.udpSendChan <- udpPacket{
		addr: addr,
		data: bytes,
	}

	// gateway stats
	if p.Payload.Stat != nil {
		b.handleStat(addr, p.GatewayMAC, *p.Payload.Stat)
	}

	// rx packets
	for _, rxpk := range p.Payload.RXPK {
		if err := b.handleRXPacket(addr, p.GatewayMAC, rxpk); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) handleStat(addr *net.UDPAddr, mac lorawan.EUI64, stat Stat) {
	gwStats := newGatewayStatsPacket(mac, stat)
	log.WithFields(log.Fields{
		"addr": addr,
		"mac":  mac,
	}).Info("gateway: stat packet received")
	b.statsChan <- gwStats
}

func (b *Backend) handleRXPacket(addr *net.UDPAddr, mac lorawan.EUI64, rxpk RXPK) error {
	logFields := log.Fields{
		"addr": addr,
		"mac":  mac,
		"data": rxpk.Data,
	}
	log.WithFields(logFields).Info("gateway: rxpk packet received")

	// decode packet
	rxPacket, err := newRXPacketFromRXPK(mac, rxpk)
	if err != nil {
		return err
	}

	// check CRC
	if rxPacket.RXInfo.CRCStatus != 1 {
		log.WithFields(logFields).Warningf("gateway: invalid packet CRC: %d", rxPacket.RXInfo.CRCStatus)
		return errors.New("gateway: invalid CRC")
	}
	b.rxChan <- rxPacket
	return nil
}

func (b *Backend) handleTXACK(addr *net.UDPAddr, data []byte) error {
	var p TXACKPacket
	if err := p.UnmarshalBinary(data); err != nil {
		return err
	}
	var errBool bool

	logFields := log.Fields{
		"mac":          p.GatewayMAC,
		"random_token": p.RandomToken,
	}
	if p.Payload != nil {
		if p.Payload.TXPKACK.Error != "NONE" {
			errBool = true
		}
		logFields["error"] = p.Payload.TXPKACK.Error
	}

	if errBool {
		log.WithFields(logFields).Error("gateway: tx ack received")
	} else {
		log.WithFields(logFields).Info("gateway: tx ack received")
	}

	return nil
}

// newGatewayStatsPacket from Stat transforms a Semtech Stat packet into a
// models.GatewayStatsPacket.
func newGatewayStatsPacket(mac lorawan.EUI64, stat Stat) models.GatewayStatsPacket {
	return models.GatewayStatsPacket{
		Time:                time.Time(stat.Time),
		MAC:                 mac,
		Latitude:            stat.Lati,
		Longitude:           stat.Long,
		Altitude:            float64(stat.Alti),
		RXPacketsReceived:   int(stat.RXNb),
		RXPacketsReceivedOK: int(stat.RXOK),
		Platform:            stat.Pfrm,
		ContactEmail:        stat.Mail,
		Description:         stat.Desc,
	}
}

// newRXPacketFromRXPK transforms a Semtech packet into a models.RXPacket.
func newRXPacketFromRXPK(mac lorawan.EUI64, rxpk RXPK) (models.RXPacket, error) {
	var phy lorawan.PHYPayload
	if err := phy.UnmarshalText([]byte(rxpk.Data)); err != nil {
		return models.RXPacket{}, fmt.Errorf("gateway: could not unmarshal PHYPayload: %s", err)
	}

	dataRate, err := newDataRateFromDatR(rxpk.DatR)
	if err != nil {
		return models.RXPacket{}, fmt.Errorf("gateway: could not get DataRate from DatR: %s", err)
	}

	rxPacket := models.RXPacket{
		PHYPayload: phy,
		RXInfo: models.RXInfo{
			MAC:       mac,
			Time:      time.Time(rxpk.Time),
			Timestamp: rxpk.Tmst,
			Frequency: int(rxpk.Freq * 1000000),
			Channel:   int(rxpk.Chan),
			RFChain:   int(rxpk.RFCh),
			CRCStatus: int(rxpk.Stat),
			DataRate:  dataRate,
			CodeRate:  rxpk.CodR,
			RSSI:      int(rxpk.RSSI),
			LoRaSNR:   rxpk.LSNR,
			Size:      int(rxpk.Size),
		},
	}
	return rxPacket, nil
}

// newTXPKFromTXPacket transforms a models.TXPacket into a Semtech
// compatible packet.
func newTXPKFromTXPacket(txPacket models.TXPacket) (TXPK, error) {
	b, err := txPacket.PHYPayload.MarshalBinary()
	if err != nil {
		return TXPK{}, err
	}

	txpk := TXPK{
		Imme: txPacket.TXInfo.Immediately,
		Tmst: txPacket.TXInfo.Timestamp,
		Freq: float64(txPacket.TXInfo.Frequency) / 1000000,
		Powe: uint8(txPacket.TXInfo.Power),
		Modu: string(txPacket.TXInfo.DataRate.Modulation),
		DatR: newDatRfromDataRate(txPacket.TXInfo.DataRate),
		CodR: txPacket.TXInfo.CodeRate,
		Size: uint16(len(b)),
		Data: base64.RawStdEncoding.EncodeToString(b),
	}

	// TODO: do testing with FSK modulation
	if txPacket.TXInfo.DataRate.Modulation == band.LoRaModulation {
		txpk.IPol = true
	}

	return txpk, nil
}

func newDataRateFromDatR(d DatR) (band.DataRate, error) {
	var dr band.DataRate

	if d.LoRa != "" {
		// parse e.g. SF12BW250 into separate variables
		match := loRaDataRateRegex.FindStringSubmatch(d.LoRa)
		if len(match) != 3 {
			return dr, errors.New("gateway: could not parse LoRa data rate")
		}

		// cast variables to ints
		sf, err := strconv.Atoi(match[1])
		if err != nil {
			return dr, fmt.Errorf("gateway: could not convert spread factor to int: %s", err)
		}
		bw, err := strconv.Atoi(match[2])
		if err != nil {
			return dr, fmt.Errorf("gateway: could not convert bandwith to int: %s", err)
		}

		dr.Modulation = band.LoRaModulation
		dr.SpreadFactor = sf
		dr.Bandwidth = bw
		return dr, nil
	}

	if d.FSK != 0 {
		dr.Modulation = band.FSKModulation
		dr.BitRate = int(d.FSK)
		return dr, nil
	}

	return dr, errors.New("gateway: could not convert DatR to DataRate, DatR is empty / modulation unknown")
}

func newDatRfromDataRate(d band.DataRate) DatR {
	if d.Modulation == band.LoRaModulation {
		return DatR{
			LoRa: fmt.Sprintf("SF%dBW%d", d.SpreadFactor, d.Bandwidth),
		}
	}

	return DatR{
		FSK: uint32(d.BitRate),
	}
}
