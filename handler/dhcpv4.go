package handler

import (
	"dhammer/config"
	"dhammer/message"
	"dhammer/socketeer"
	"dhammer/stats"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"net"
	"time"
)

type LeaseDhcpV4 struct {
	Packet   gopacket.Packet
	LinkAddr *netlink.Addr
	Acquired time.Time
	HwAddr   net.HardwareAddr
}

type HandlerDhcpV4 struct {
	options      *config.DhcpV4Options
	socketeer    *socketeer.RawSocketeer
	iface        *net.Interface
	link         netlink.Link
	acquiredIPs  map[string]*LeaseDhcpV4
	addLog       func(string) bool
	addError     func(error) bool
	sendPayload  func([]byte) bool
	addStat      func(stats.StatValue) bool
	inputChannel chan message.Message
	doneChannel  chan struct{}
}

func init() {
	if err := AddHandler("dhcpv4", NewDhcpV4); err != nil {
		panic(err)
	}
}

func NewDhcpV4(hip HandlerInitParams) Handler {

	h := HandlerDhcpV4{
		options:      hip.options.(*config.DhcpV4Options),
		socketeer:    hip.socketeer,
		iface:        hip.socketeer.IfInfo,
		acquiredIPs:  make(map[string]*LeaseDhcpV4),
		addLog:       hip.logFunc,
		addError:     hip.errFunc,
		sendPayload:  hip.socketeer.AddPayload,
		addStat:      hip.statFunc,
		inputChannel: make(chan message.Message, 10000),
		doneChannel:  make(chan struct{}),
	}

	return &h
}

func (h *HandlerDhcpV4) ReceiveMessage(msg message.Message) bool {

	select {
	case h.inputChannel <- msg:
		return true
	default:
	}

	return false

}

func (h *HandlerDhcpV4) Init() error {

	var err error = nil

	h.link, err = netlink.LinkByName("lo")

	return err
}

func (h *HandlerDhcpV4) DeInit() error {

	if h.options.Bind {
		for _, lease := range h.acquiredIPs {
			if err := netlink.AddrDel(h.link, lease.LinkAddr); err != nil {
				h.addError(err)
			}
		}
	}

	return nil
}

func (h *HandlerDhcpV4) Stop() error {
	close(h.inputChannel)
	<-h.doneChannel
	return nil
}

func (h *HandlerDhcpV4) Run() {

	var msg message.Message
	var dhcpReply *layers.DHCPv4

	socketeerOptions := h.socketeer.Options()

	ethernetLayer := &layers.Ethernet{
		DstMAC:       layers.EthernetBroadcast,
		SrcMAC:       h.iface.HardwareAddr,
		EthernetType: layers.EthernetTypeIPv4,
		Length:       0,
	}

	if !h.options.EthernetBroadcast {
		ethernetLayer.DstMAC = socketeerOptions.GatewayMAC
	}

	ipLayer := &layers.IPv4{
		Version:  4, // IPv4
		TTL:      64,
		Protocol: 17, // UDP
		SrcIP:    net.IPv4(0, 0, 0, 0),
		DstIP:    net.IPv4(255, 255, 255, 255),
	}

	udpLayer := &layers.UDP{
		SrcPort: layers.UDPPort(68),
		DstPort: layers.UDPPort(h.options.TargetPort),
	}

	outDhcpLayer := &layers.DHCPv4{
		Operation:    layers.DHCPOpRequest,
		HardwareType: layers.LinkTypeEthernet,
		HardwareLen:  6,
		Flags:        0x8000, // Broadcast
	}

	if !h.options.DhcpBroadcast {
		outDhcpLayer.Flags = 0x0
	}

	if h.options.DhcpRelay {
		ipLayer.SrcIP = h.options.RelaySourceIP
		ipLayer.DstIP = h.options.RelayTargetServerIP

		ethernetLayer.SrcMAC = h.iface.HardwareAddr
		ethernetLayer.DstMAC = socketeerOptions.GatewayMAC

		outDhcpLayer.RelayAgentIP = h.options.RelayGatewayIP
		udpLayer.SrcPort = 67
	}

	goPacketSerializeOpts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}

	for msg = range h.inputChannel {

		if h.options.Arp && msg.Packet.Layer(layers.LayerTypeARP) != nil {
			h.addStat(stats.ArpRequestReceivedStat)
			h.handleARP(msg)
			continue
		} else if msg.Packet.Layer(layers.LayerTypeDHCPv4) == nil {
			continue
		}

		dhcpReply = msg.Packet.Layer(layers.LayerTypeDHCPv4).(*layers.DHCPv4)

		var replyOptions [256]layers.DHCPOption

		for _, option := range dhcpReply.Options { // Assuming that we'll expand on usage of options in the reply later and just doing this now.
			replyOptions[option.Type] = option
		}

		replyMsgType := replyOptions[layers.DHCPOptMessageType].Data[0]

		//h.addLog(fmt.Sprintf("[REPLY] %v %v %v %v %v", dhcpReply.Options[0].String(), dhcpReply.YourClientIP.String(), string(dhcpReply.ServerName), dhcpReply.ClientIP.String(), dhcpReply.ClientHWAddr))

		if replyMsgType == (byte)(layers.DHCPMsgTypeOffer) {

			h.addStat(stats.OfferReceivedStat)

			if h.options.Handshake {

				buf := gopacket.NewSerializeBuffer()

				outDhcpLayer.Xid = dhcpReply.Xid

				outDhcpLayer.Options = make(layers.DHCPOptions, 4)

				if h.options.DhcpDecline {
					outDhcpLayer.Options[0] = layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeDecline)})
				} else {
					outDhcpLayer.Options[0] = layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeRequest)})
				}

				outDhcpLayer.Options[1] = layers.NewDHCPOption(layers.DHCPOptRequestIP, dhcpReply.YourClientIP)
				outDhcpLayer.Options[2] = layers.NewDHCPOption(layers.DHCPOptServerID, replyOptions[layers.DHCPOptServerID].Data)
				outDhcpLayer.Options[3] = layers.NewDHCPOption(layers.DHCPOptEnd, []byte{})

				outDhcpLayer.ClientHWAddr = dhcpReply.ClientHWAddr

				udpLayer.SetNetworkLayerForChecksum(ipLayer)

				gopacket.SerializeLayers(buf, goPacketSerializeOpts,
					ethernetLayer,
					ipLayer,
					udpLayer,
					outDhcpLayer,
				)

				if h.sendPayload(buf.Bytes()) {
					if h.options.DhcpDecline {
						h.addStat(stats.DeclineSentStat)
					} else {
						h.addStat(stats.RequestSentStat)
					}
				}
			}
		} else if replyMsgType == (byte)(layers.DHCPMsgTypeAck) {

			h.addStat(stats.AckReceivedStat)

			if h.options.Arp || h.options.Bind {

				ipStr := dhcpReply.YourClientIP.String()

				if _, found := h.acquiredIPs[ipStr]; !found {

					h.acquiredIPs[ipStr] = &LeaseDhcpV4{
						Packet:   msg.Packet,
						Acquired: time.Now(),
						HwAddr:   dhcpReply.ClientHWAddr,
					}

					if h.options.Bind {

						// Need to fix the CIDR here...
						if addr, err := netlink.ParseAddr(ipStr + "/32"); err != nil {
							h.addError(err)
						} else if err = netlink.AddrAdd(h.link, addr); err != nil {
							h.addError(err)
						} else {
							h.acquiredIPs[ipStr].LinkAddr = addr
						}
					}
				}
			}

			if h.options.DhcpRelease || h.options.DhcpInfo {

				buf := gopacket.NewSerializeBuffer()

				outDhcpLayer.Xid = dhcpReply.Xid

				/* We have to unicast DHCPRELEASE - https://tools.ietf.org/html/rfc2131#section-4.4.4 */

				dhcpReplyEtherFrame := msg.Packet.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)

				/*
					The "next server" value of the DHCP reply might not actually be the server issuing the IP.
					Not seeing another sure option for grabbing the DHCP server IP aside from yanking it out of the IP header.
				*/

				dhcpReplyIpHeader := msg.Packet.Layer(layers.LayerTypeIPv4).(*layers.IPv4)

				releaseEthernetLayer := &layers.Ethernet{
					DstMAC:       dhcpReplyEtherFrame.SrcMAC,
					SrcMAC:       h.iface.HardwareAddr,
					EthernetType: layers.EthernetTypeIPv4,
					Length:       0,
				}

				releaseIpLayer := &layers.IPv4{
					Version:  4, // IPv4
					TTL:      64,
					Protocol: 17, // UDP
					SrcIP:    dhcpReply.YourClientIP,
					DstIP:    dhcpReplyIpHeader.SrcIP,
				}

				previousClientIP := outDhcpLayer.ClientIP
				outDhcpLayer.ClientIP = dhcpReply.YourClientIP

				outDhcpLayer.Options = make(layers.DHCPOptions, 2)

				previousFlags := outDhcpLayer.Flags
				outDhcpLayer.Flags = 0x0

				if h.options.DhcpInfo {
					outDhcpLayer.Options[0] = layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeInform)})
				} else {
					outDhcpLayer.Options[0] = layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeRelease)})
				}
				outDhcpLayer.Options[1] = layers.NewDHCPOption(layers.DHCPOptEnd, []byte{})

				outDhcpLayer.ClientHWAddr = dhcpReply.ClientHWAddr

				udpLayer.SetNetworkLayerForChecksum(ipLayer)

				gopacket.SerializeLayers(buf, goPacketSerializeOpts,
					releaseEthernetLayer,
					releaseIpLayer,
					udpLayer,
					outDhcpLayer,
				)

				// Reset ClientIP to what it was.  It might have been an IP or it might have been 0.0.0.0, depending what options were used.
				outDhcpLayer.ClientIP = previousClientIP
				// Similarly for flags.
				outDhcpLayer.Flags = previousFlags

				if h.sendPayload(buf.Bytes()) {
					if h.options.DhcpInfo {
						h.addStat(stats.InfoSentStat)
					} else {
						h.addStat(stats.ReleaseSentStat)
					}
				}
			}

		} else if dhcpReply.Options[0].Data[0] == (byte)(layers.DHCPMsgTypeNak) {
			h.addStat(stats.NakReceivedStat)
		}
	}

	h.doneChannel <- struct{}{}
}

func (h *HandlerDhcpV4) handleARP(msg message.Message) {
	arpRequest := msg.Packet.Layer(layers.LayerTypeARP).(*layers.ARP)

	if arpRequest.Operation == layers.ARPRequest {
		if lease, found := h.acquiredIPs[net.IP(arpRequest.DstProtAddress).String()]; found {

			goPacketSerializeOpts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}

			ethernetLayer := &layers.Ethernet{
				DstMAC:       net.HardwareAddr(arpRequest.SourceHwAddress),
				SrcMAC:       h.iface.HardwareAddr,
				EthernetType: layers.EthernetTypeARP,
				Length:       0,
			}

			arpLayer := &layers.ARP{
				Operation:         layers.ARPReply,
				DstHwAddress:      arpRequest.SourceHwAddress,
				DstProtAddress:    arpRequest.SourceProtAddress,
				HwAddressSize:     arpRequest.HwAddressSize,
				AddrType:          arpRequest.AddrType,
				ProtAddressSize:   arpRequest.ProtAddressSize,
				Protocol:          arpRequest.Protocol,
				SourceHwAddress:   h.iface.HardwareAddr,
				SourceProtAddress: arpRequest.DstProtAddress,
			}

			if h.options.ArpFakeMAC {
				arpLayer.SourceHwAddress = lease.HwAddr
			}

			buf := gopacket.NewSerializeBuffer()

			gopacket.SerializeLayers(buf, goPacketSerializeOpts,
				ethernetLayer,
				arpLayer,
			)

			if h.sendPayload(buf.Bytes()) {
				h.addStat(stats.ArpReplySentStat)
			}
		}
	}
}
