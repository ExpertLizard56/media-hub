package main

// Finding the HDHomeRun on the home network, the way SiliconDust's own apps do: a discovery
// request broadcast to UDP port 65001, which every HDHomeRun answers. (SiliconDust's cloud
// directory lists nothing for a plain network.) It needs the add-on on the host network,
// so the broadcast reaches the LAN.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"time"
)

const (
	discoverPort    = 65001
	discoverRequest = 0x0002
	discoverReply   = 0x0003
	tagDeviceType   = 0x01
	tagDeviceID     = 0x02
	tagBaseURL      = 0x2A
	wildcard        = 0xFFFFFFFF
)

// discoverTuner broadcasts a discovery request and returns the first HDHomeRun's base URL.
func discoverTuner() (string, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", err
	}
	defer closeQuietly(conn) // it only sends and listens
	request := discoveryPacket()
	target := &net.UDPAddr{IP: net.IPv4bcast, Port: discoverPort}
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 2048)
	for attempt := 0; attempt < 3 && time.Now().Before(deadline); attempt++ {
		if _, err := conn.WriteTo(request, target); err != nil {
			return "", fmt.Errorf("broadcast: %w", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			return "", fmt.Errorf("discovery: %w", err)
		}
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				break // timed out: send again
			}
			if base, ok := parseDiscoveryReply(buf[:n], from); ok {
				return base, nil
			}
		}
	}
	return "", errors.New("no HDHomeRun answered on the home network")
}

// discoveryPacket asks every device of every type to answer.
func discoveryPacket() []byte {
	var payload bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	for _, tag := range []byte{tagDeviceType, tagDeviceID} {
		payload.WriteByte(tag)
		payload.WriteByte(4)
		_ = binary.Write(&payload, binary.BigEndian, uint32(wildcard))
	}
	return framePacket(discoverRequest, payload.Bytes())
}

// framePacket: type and payload length (big-endian), the payload, then a CRC-32 of all that (little-endian).
func framePacket(kind uint16, payload []byte) []byte {
	var p bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	_ = binary.Write(&p, binary.BigEndian, kind)
	_ = binary.Write(&p, binary.BigEndian, uint16(len(payload)))
	p.Write(payload)
	_ = binary.Write(&p, binary.LittleEndian, crc32.ChecksumIEEE(p.Bytes()))
	return p.Bytes()
}

// parseDiscoveryReply reads a device's answer: its base URL if it gives one, else its address.
func parseDiscoveryReply(packet []byte, from net.Addr) (string, bool) {
	if len(packet) < 8 || binary.BigEndian.Uint16(packet[0:2]) != discoverReply {
		return "", false
	}
	length := int(binary.BigEndian.Uint16(packet[2:4]))
	if len(packet) < 4+length+4 {
		return "", false
	}
	if crc32.ChecksumIEEE(packet[:4+length]) != binary.LittleEndian.Uint32(packet[4+length:]) {
		return "", false
	}
	payload := packet[4 : 4+length]
	for len(payload) >= 2 {
		tag := payload[0]
		size := int(payload[1])
		offset := 2
		if size&0x80 != 0 { // two-byte length
			if len(payload) < 3 {
				break
			}
			size = (size & 0x7F) | int(payload[2])<<7
			offset = 3
		}
		if len(payload) < offset+size {
			break
		}
		if tag == tagBaseURL {
			return string(payload[offset : offset+size]), true
		}
		payload = payload[offset+size:]
	}
	if udp, ok := from.(*net.UDPAddr); ok {
		return "http://" + udp.IP.String(), true
	}
	return "", false
}
