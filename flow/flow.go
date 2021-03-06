package flow

import (
	"fmt"
	"jakub-m/bdp/packet"
	"jakub-m/bdp/pcap"
	"log"
)

const (
	usecInSec = 1000 * 1000
	csvHeader = "# bandwidth (bps)\trtt (usec)\twindow sent\twindow ack"
)

// ProcessPackets iterates all the packets and produces RTT and bandwidth statistics.
func ProcessPackets(packets []*packet.Packet, localIP, remoteIP *pcap.IPv4) error {
	flow := &flow{}

	fmt.Println(csvHeader)
	for _, f := range packets {
		if fp, err := flow.consumePacket(f, localIP, remoteIP); err == nil {
			log.Println(fp.String())
		} else {
			log.Println(err)
		}
	}
	return nil
}

// initTimestamp initial timestamp in microseconds
// local is the side that initiates connection (syn).
// remote is the other side of the connection (syn ack).
// inflight are the files that are sent from local to remote and are not yet acknowledged.
// deliveredTime is time of the most recent ACK, as in BBR paper.
// delivered is sum of bytes delivered, as in BBR paper.
type flow struct {
	initTimestamp uint64
	local         *flowDetails
	remote        *flowDetails
	inflight      []*flowPacket
	stats         []*flowStat
	deliveredTime uint64
	delivered     uint32
	cbAckInFlight func(*flowStat)
}

// initSeqNum is initial sequence number.
type flowDetails struct {
	ip         pcap.IPv4
	initSeqNum pcap.SeqNum
}

// flowPacket is a packet.Packet with flow context
type flowPacket struct {
	relativeTimestamp uint64
	packet            *packet.Packet
	direction         flowPacketDirection
	relativeSeqNum    pcap.SeqNum
	relativeAckNum    pcap.SeqNum
	expectedAckNum    pcap.SeqNum
	deliveredTime     uint64
	delivered         uint32
}

type flowPacketDirection int

const (
	localToRemote flowPacketDirection = iota
	remoteToLocal
)

func (f *flow) consumePacket(packet *packet.Packet, localIP, remoteIP *pcap.IPv4) (*flowPacket, error) {
	if !((packet.IP.SourceIP() == *localIP && packet.IP.DestIP() == *remoteIP) ||
		(packet.IP.SourceIP() == *remoteIP && packet.IP.DestIP() == *localIP)) {
		// Filter packets that surely do not belong to the flow.
		return nil, fmt.Errorf("Dropping %s > %s (not in the flow)", packet.IP.SourceIP(), packet.IP.DestIP())
	}

	if f.local == nil && f.remote == nil {
		// If has neither local or remote, treat the first packet as local packet.
		if packet.IP.SourceIP() != *localIP {
			return nil, fmt.Errorf("Dropping %s > %s (not local-to-remote)", packet.IP.SourceIP(), packet.IP.DestIP())
		}

		f.initTimestamp = packet.Record.Timestamp()
		f.local = newFlowDetailsFromSource(packet) // TODO Simplify, since local IP is known.
		fp := f.newInitialFlowPacket(packet, localToRemote)
		log.Printf("Initialize local: %s", fp)
		return fp, nil
	} else if f.local != nil && f.remote == nil {
		// If has only local, either set remote (in case of remote-to-local packet), or update local (in case
		// of local-to-remote packet).
		if f.local.ip == packet.IP.SourceIP() {
			f.local = newFlowDetailsFromSource(packet)
			fp := f.newInitialFlowPacket(packet, localToRemote)
			log.Printf("Update local: %s", fp)
			return fp, nil
		} else {
			f.remote = newFlowDetailsFromSource(packet)
			fp := f.newInitialFlowPacket(packet, remoteToLocal)
			log.Printf("Initialize remote: %s", fp)
			return fp, nil
		}
	} else if f.local != nil && f.remote != nil {
		flowPacket, err := f.createFlowPacket(packet)
		if err != nil {
			return nil, err
		}
		// If has both local and remote, do the proper processing.
		if flowPacket.direction == localToRemote {
			err := f.onSend(flowPacket)
			if err != nil {
				return nil, err
			}
		} else if flowPacket.direction == remoteToLocal && flowPacket.packet.TCP.IsAck() {
			f.onAck(flowPacket)
		} else {
			panic("direction not set!")
		}
		return flowPacket, nil
	}
	panic(fmt.Sprintf("BAD STATE, f.local=%+v, f.remote=%+v", f.local, f.remote))
}

// Packets sent are inflight until acknowledged. Only packets with payload are expected to be acknowledged (i.e. pure 'acks' with no payload do not count as inflight.)
func (f *flow) onSend(p *flowPacket) error {
	if p.packet.PayloadSize() == 0 {
		return nil
	}
	// Assert that packets are sorted by expectedAckNum.
	if len(f.inflight) > 0 {
		lastInflight := f.inflight[len(f.inflight)-1]
		if lastInflight.expectedAckNum >= p.expectedAckNum {
			return fmt.Errorf("Wrong order of expectedAckNum. last inflight %s, current %s", lastInflight, p)
		}
	}
	p.delivered = f.delivered
	p.deliveredTime = f.deliveredTime
	f.inflight = append(f.inflight, p)
	return nil
}

func (f *flow) onAck(ack *flowPacket) {
	sent, i, ok := f.findPacketSent(ack)
	if !ok {
		return
	}

	rtt := ack.packet.Record.Timestamp() - sent.packet.Record.Timestamp()
	f.delivered += uint32(sent.packet.PayloadSize())
	f.deliveredTime = ack.packet.Record.Timestamp()
	deliveryRate := 8 * usecInSec * float32(f.delivered-sent.delivered) / float32(f.deliveredTime-sent.deliveredTime)

	stat := &flowStat{
		// Note that relativeTimestampUSec is the timestmap of the ACK-ing packet, not the original packet.
		relativeTimestampUSec: ack.relativeTimestamp,
		rttUSec:               rtt,
		deliveryRateBPS:       uint32(deliveryRate),
		sentWindowSize:        sent.packet.TCP.WindowSize(),
		ackWindowSize:         ack.packet.TCP.WindowSize(),
	}
	log.Printf("Got ack for inflight packet: ackNum=%d, rate=%.0fkb/s, %s", ack.relativeAckNum, deliveryRate/1000, stat)
	fmt.Println(stat.CSVString()) // Eventually remove it from here to a delegated callback
	f.stats = append(f.stats, stat)
	f.inflight = f.inflight[i+1:]
}

func (f *flow) findPacketSent(ack *flowPacket) (sent *flowPacket, inflightIndex int, ok bool) {
	for i, g := range f.inflight {
		if ack.relativeAckNum == g.expectedAckNum {
			return g, i, true
		}
	}
	return nil, -1, false
}

func (f *flow) newInitialFlowPacket(packet *packet.Packet, direction flowPacketDirection) *flowPacket {
	return &flowPacket{
		packet:            packet,
		relativeTimestamp: f.getRelativeTimestamp(packet),
		direction:         direction,
		relativeSeqNum:    0,
		relativeAckNum:    0,
		expectedAckNum:    1,
	}
}

func (f *flow) createFlowPacket(packet *packet.Packet) (*flowPacket, error) {
	if f.local == nil || f.remote == nil {
		panic("local or remote is nil")
	}

	flowPacket := &flowPacket{
		packet:            packet,
		relativeTimestamp: f.getRelativeTimestamp(packet),
	}

	if f.isLocalToRemote(packet) {
		flowPacket.direction = localToRemote
		flowPacket.relativeSeqNum = packet.TCP.SeqNum().RelativeTo(f.local.initSeqNum)
		flowPacket.relativeAckNum = packet.TCP.AckNum().RelativeTo(f.remote.initSeqNum)
		// FIXME: handle next syn at integer boundaries gracefully.
		flowPacket.expectedAckNum = flowPacket.relativeSeqNum.ExpectedForPayload(packet.PayloadSize())
	} else if f.isRemoteToLocal(packet) {
		flowPacket.direction = remoteToLocal
		flowPacket.relativeSeqNum = packet.TCP.SeqNum().RelativeTo(f.remote.initSeqNum)
		flowPacket.relativeAckNum = packet.TCP.AckNum().RelativeTo(f.local.initSeqNum)
		flowPacket.expectedAckNum = flowPacket.relativeSeqNum.ExpectedForPayload(packet.PayloadSize())
	} else {
		return nil, fmt.Errorf("Unknown direction! %s", packet)
	}

	return flowPacket, nil
}

func (f *flow) getRelativeTimestamp(packet *packet.Packet) uint64 {
	return packet.Record.Timestamp() - f.initTimestamp
}

// isLocalToRemote indicates if a packet represents a packet going from local to remote.
func (f *flow) isLocalToRemote(packet *packet.Packet) bool {
	return f.local.ip == packet.IP.SourceIP() && f.remote.ip == packet.IP.DestIP()
}

// isRemoteToLocal indicates if a packet represents a packet going from remote to local.
func (f *flow) isRemoteToLocal(packet *packet.Packet) bool {
	return f.remote.ip == packet.IP.SourceIP() && f.local.ip == packet.IP.DestIP()
}

func (p *flowPacket) String() string {
	msg := fmt.Sprintf("%d", p.relativeTimestamp)

	if p.direction == localToRemote {
		msg += fmt.Sprintf(" %s >  %s", p.packet.IP.SourceIP(), p.packet.IP.DestIP())
	} else if p.direction == remoteToLocal {
		msg += fmt.Sprintf(" %s  < %s", p.packet.IP.DestIP(), p.packet.IP.SourceIP())
	}
	if p.packet.TCP.IsSyn() {
		msg += " syn"
	}
	if p.packet.TCP.IsAck() {
		msg += " ack"
	}

	msg += fmt.Sprintf(" %d. seq %d (exp %d) ack %d", p.packet.PayloadSize(), p.relativeSeqNum, p.expectedAckNum, p.relativeAckNum)
	return msg
}

// newFlowDetailsFromSource creates *flowDetails from source of the packet (that is, not from destination).
func newFlowDetailsFromSource(packet *packet.Packet) *flowDetails {
	return &flowDetails{
		ip:         packet.IP.SourceIP(),
		initSeqNum: packet.TCP.SeqNum(),
	}
}

func (d *flowDetails) String() string {
	return fmt.Sprintf("%s, seq: %d", d.ip, d.initSeqNum)
}

// Single data point for flow statistics.
type flowStat struct {
	relativeTimestampUSec uint64
	rttUSec               uint64
	deliveryRateBPS       uint32
	sentWindowSize        uint16
	ackWindowSize         uint16
}

func (s *flowStat) String() string {
	return fmt.Sprintf("ts: %d msec, rtt: %d msec, win: %d, %d", s.relativeTimestampUSec/1000, s.rttUSec/1000, s.sentWindowSize, s.ackWindowSize)
}

func (s *flowStat) CSVString() string {
	return fmt.Sprintf("%d\t%d\t%d\t%d", s.deliveryRateBPS, s.rttUSec, s.sentWindowSize, s.ackWindowSize)
}
