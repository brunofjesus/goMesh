package gomesh

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"time"

	pb "github.com/lmatte7/gomesh/github.com/meshtastic/gomeshproto"
	"google.golang.org/protobuf/proto"
)

const (
	start1             = byte(0x94)
	start2             = byte(0xc3)
	headerLen          = 4
	maxToFromRadioSzie = 512
	broadcastAddr      = "^all"
	localAddr          = "^local"
	defaultHopLimit    = 3
	broadcastNum       = 0xffffffff
)

// Radio holds the port and serial io.ReadWriteCloser struct to maintain one serial connection
type Radio struct {
	streamer streamer
}

// Init initializes the Serial connection for the radio
func (r *Radio) Init(port string) error {
	streamer := streamer{}
	err := streamer.Init(port)
	if err != nil {
		return err
	}
	r.streamer = streamer

	return nil
}

// sendPacket takes a protbuf packet, construct the appropriate header and sends it to the radio
func (r *Radio) sendPacket(protobufPacket []byte) (err error) {
	packageLength := len(string(protobufPacket))

	header := []byte{start1, start2, byte(packageLength>>8) & 0xff, byte(packageLength) & 0xff}

	radioPacket := append(header, protobufPacket...)
	err = r.streamer.Write(radioPacket)
	if err != nil {
		return err
	}

	return
}

// ReadResponse reads any responses in the serial port, convert them to a FromRadio protobuf and return
func (r *Radio) ReadResponse(timeout bool) (FromRadioPackets []*pb.FromRadio, err error) {
	b := make([]byte, 1)

	emptyByte := make([]byte, 0)
	processedBytes := make([]byte, 0)
	repeatByteCounter := 0
	previousByte := make([]byte, 1)
	/************************************************************************************************
	* Process the returned data byte by byte until we have a valid command
	* Each command will come back with [START1, START2, PROTOBUF_PACKET]
	* where the protobuf packet is sent in binary. After reading START1 and START2
	* we use the next bytes to find the length of the packet.
	* After finding the length the looop continues to gather bytes until the length of the gathered
	* bytes is equal to the packet length plus the header
	 */
	for {
		err := r.streamer.Read(b)
		// fmt.Printf("Byte: %q\n", b)
		if bytes.Equal(b, previousByte) {
			repeatByteCounter++
		} else {
			repeatByteCounter = 0
		}

		if err == io.EOF || repeatByteCounter > 20 || errors.Is(err, os.ErrDeadlineExceeded) {
			break
		} else if err != nil {
			return nil, err
		}
		copy(previousByte, b)

		if len(b) > 0 {

			pointer := len(processedBytes)

			processedBytes = append(processedBytes, b...)

			if pointer == 0 {
				if b[0] != start1 {
					processedBytes = emptyByte
				}
			} else if pointer == 1 {
				if b[0] != start2 {
					processedBytes = emptyByte
				}
			} else if pointer >= headerLen {
				packetLength := int((processedBytes[2] << 8) + processedBytes[3])

				if pointer == headerLen {
					if packetLength > maxToFromRadioSzie {
						processedBytes = emptyByte
					}
				}

				if len(processedBytes) != 0 && pointer+1 == packetLength+headerLen {
					fromRadio := pb.FromRadio{}
					if err := proto.Unmarshal(processedBytes[headerLen:], &fromRadio); err != nil {
						return nil, err
					}
					FromRadioPackets = append(FromRadioPackets, &fromRadio)
					processedBytes = emptyByte
				}
			}

		} else {
			break
		}

	}

	return FromRadioPackets, nil
}

// createAdminPacket builds a admin message packet to send to the radio
func (r *Radio) createAdminPacket(nodeNum uint32, payload []byte) (packetOut []byte, err error) {
	radioMessage := pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To:      nodeNum,
				WantAck: true,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Payload:      payload,
						Portnum:      pb.PortNum_ADMIN_APP,
						WantResponse: true,
					},
				},
			},
		},
	}

	packetOut, err = proto.Marshal(&radioMessage)
	if err != nil {
		return nil, err
	}

	return
}

// getNodeNum returns the current NodeNumber after querying the radio
func (r *Radio) getNodeNum() (nodeNum uint32, err error) {
	// Send first request for Radio and Node information
	nodeInfo := pb.ToRadio{PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 42}}

	out, err := proto.Marshal(&nodeInfo)
	if err != nil {
		return 0, err
	}

	r.sendPacket(out)

	radioResponses, err := r.ReadResponse(true)
	if err != nil {
		return 0, err
	}

	// Gather the Node number for channel settings requests
	nodeNum = 0
	for _, response := range radioResponses {
		if info, ok := response.GetPayloadVariant().(*pb.FromRadio_MyInfo); ok {
			nodeNum = info.MyInfo.MyNodeNum
		}
	}

	return
}

// GetRadioInfo retrieves information from the radio including config and adjacent Node information
func (r *Radio) GetRadioInfo() (radioResponses []*pb.FromRadio, err error) {
	// Send first request for Radio and Node information
	nodeInfo := pb.ToRadio{PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 42}}

	out, err := proto.Marshal(&nodeInfo)
	if err != nil {
		return nil, err
	}

	r.sendPacket(out)

	var responses []*pb.FromRadio
	for i := 0; i < 10; i++ {
		responses, err = r.ReadResponse(true)
		radioResponses = append(radioResponses, responses...)
		for _, response := range responses {
			if configId, ok := response.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId); ok && configId.ConfigCompleteId == 42 {
				return responses, err
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	return
}

// SendTextMessage sends a free form text message to other radios
func (r *Radio) SendTextMessage(message string, to int64, channel int64) error {
	return r.SendDecodedMessage(pb.PortNum_TEXT_MESSAGE_APP, []byte(message), to, channel, true)
}

// SendIpTunnelMessage sends a IP_TUNNEL_APP message
func (r *Radio) SendIpTunnelMessage(payload []byte, to int64, channel int64) error {
	return r.SendDecodedMessage(pb.PortNum_IP_TUNNEL_APP, payload, to, channel, false)
}

// SendDecodedMessage sends a message with a decoded packet
func (r *Radio) SendDecodedMessage(portnum pb.PortNum, payload []byte, to int64, channel int64, wantAck bool) error {
	var address int64
	if to == 0 {
		address = broadcastNum
	} else {
		address = to
	}

	// This constant is defined in Constants_DATA_PAYLOAD_LEN, but not in a friendly way to use
	if len(payload) > 240 {
		return errors.New("message too large")
	}

	rand.Seed(time.Now().UnixNano())
	packetID := rand.Intn(2386828-1) + 1

	radioMessage := pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To:      uint32(address),
				WantAck: wantAck,
				Id:      uint32(packetID),
				Channel: uint32(channel),
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Payload: payload,
						Portnum: portnum,
					},
				},
			},
		},
	}

	return r.SendMessage(&radioMessage)
}

// SendMessage marshals and sends a ToRadio packet
func (r *Radio) SendMessage(radioMessage *pb.ToRadio) error {
	out, err := proto.Marshal(radioMessage)
	if err != nil {
		return err
	}

	return r.sendPacket(out)
}

// SetRadioOwner sets the owner of the radio visible on the public mesh
func (r *Radio) SetRadioOwner(name string) error {
	if len(name) <= 2 {
		return errors.New("name too short")
	}

	adminPacket := pb.AdminMessage{
		PayloadVariant: &pb.AdminMessage_SetOwner{
			SetOwner: &pb.User{
				LongName:  name,
				ShortName: name[:3],
			},
		},
	}

	out, err := proto.Marshal(&adminPacket)
	if err != nil {
		return err
	}

	nodeNum, err := r.getNodeNum()
	if err != nil {
		return err
	}
	packet, err := r.createAdminPacket(nodeNum, out)
	if err != nil {
		return err
	}

	if err := r.sendPacket(packet); err != nil {
		return err
	}

	return nil
}

// SetModemMode sets the channel modem setting to be fast or slow
func (r *Radio) SetModemMode(mode string) error {
	var modemSetting pb.Config_LoRaConfig_ModemPreset

	if mode == "lf" {
		modemSetting = pb.Config_LoRaConfig_LONG_FAST
	} else if mode == "ls" {
		modemSetting = pb.Config_LoRaConfig_LONG_SLOW
	} else if mode == "vls" {
		modemSetting = pb.Config_LoRaConfig_VERY_LONG_SLOW
	} else if mode == "ms" {
		modemSetting = pb.Config_LoRaConfig_MEDIUM_SLOW
	} else if mode == "mf" {
		modemSetting = pb.Config_LoRaConfig_MEDIUM_FAST
	} else if mode == "sl" {
		modemSetting = pb.Config_LoRaConfig_SHORT_SLOW
	} else if mode == "sf" {
		modemSetting = pb.Config_LoRaConfig_SHORT_FAST
	} else if mode == "lm" {
		modemSetting = pb.Config_LoRaConfig_LONG_MODERATE
	}

	adminPacket := pb.AdminMessage{
		PayloadVariant: &pb.AdminMessage_SetConfig{
			SetConfig: &pb.Config{
				PayloadVariant: &pb.Config_Lora{
					Lora: &pb.Config_LoRaConfig{
						ModemPreset: modemSetting,
					},
				},
			},
		},
	}

	out, err := proto.Marshal(&adminPacket)
	if err != nil {
		return err
	}

	nodeNum, err := r.getNodeNum()
	if err != nil {
		return err
	}
	packet, err := r.createAdminPacket(nodeNum, out)
	if err != nil {
		return err
	}

	if err := r.sendPacket(packet); err != nil {
		return err
	}

	return nil
}

// SetLocation sets a fixed location for the radio
func (r *Radio) SetLocation(lat float64, long float64, alt int32) error {
	positionPacket := pb.Position{
		LatitudeI:  int32(lat),
		LongitudeI: int32(long),
		Altitude:   int32(alt),
		Time:       0,
	}

	out, err := proto.Marshal(&positionPacket)
	if err != nil {
		return err
	}

	nodeNum, err := r.getNodeNum()
	if err != nil {
		return err
	}

	radioMessage := pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To:      nodeNum,
				WantAck: true,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Payload:      out,
						Portnum:      pb.PortNum_POSITION_APP,
						WantResponse: true,
					},
				},
			},
		},
	}

	packet, err := proto.Marshal(&radioMessage)
	if err != nil {
		return err
	}

	if err := r.sendPacket(packet); err != nil {
		return err
	}

	return nil
}

// Send a factory reset command to the radio
func (r *Radio) FactoryRest() error {
	adminPacket := pb.AdminMessage{
		PayloadVariant: &pb.AdminMessage_FactoryReset{
			FactoryReset: 1,
		},
	}
	out, err := proto.Marshal(&adminPacket)
	if err != nil {
		return err
	}

	nodeNum, err := r.getNodeNum()
	if err != nil {
		return err
	}
	packet, err := r.createAdminPacket(nodeNum, out)
	if err != nil {
		return err
	}

	if err := r.sendPacket(packet); err != nil {
		return err
	}

	return nil
}

// Close closes the serial port. Added so users can defer the close after opening
func (r *Radio) Close() {
	r.streamer.Close()
}
