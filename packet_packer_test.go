package quic

import (
	"bytes"
	"math"

	"github.com/lucas-clemente/quic-go/ackhandler"
	"github.com/lucas-clemente/quic-go/frames"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/internal/mocks"
	"github.com/lucas-clemente/quic-go/protocol"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type mockCryptoSetup struct {
	handleErr          error
	divNonce           []byte
	encLevelSeal       protocol.EncryptionLevel
	encLevelSealCrypto protocol.EncryptionLevel
}

func (m *mockCryptoSetup) HandleCryptoStream() error {
	return m.handleErr
}
func (m *mockCryptoSetup) Open(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) ([]byte, protocol.EncryptionLevel, error) {
	return nil, protocol.EncryptionUnspecified, nil
}
func (m *mockCryptoSetup) GetSealer() (protocol.EncryptionLevel, handshake.Sealer) {
	return m.encLevelSeal, func(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) []byte {
		return append(src, bytes.Repeat([]byte{0}, 12)...)
	}
}
func (m *mockCryptoSetup) GetSealerForCryptoStream() (protocol.EncryptionLevel, handshake.Sealer) {
	return m.encLevelSealCrypto, func(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) []byte {
		return append(src, bytes.Repeat([]byte{0}, 12)...)
	}
}
func (m *mockCryptoSetup) GetSealerWithEncryptionLevel(protocol.EncryptionLevel) (handshake.Sealer, error) {
	return func(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) []byte {
		return append(src, bytes.Repeat([]byte{0}, 12)...)
	}, nil
}
func (m *mockCryptoSetup) DiversificationNonce() []byte            { return m.divNonce }
func (m *mockCryptoSetup) SetDiversificationNonce(divNonce []byte) { m.divNonce = divNonce }

var _ handshake.CryptoSetup = &mockCryptoSetup{}

var _ = Describe("Packet packer", func() {
	var (
		packer          *packetPacker
		publicHeaderLen protocol.ByteCount
		maxFrameSize    protocol.ByteCount
		streamFramer    *streamFramer
		cryptoStream    *stream
	)

	BeforeEach(func() {
		mockCpm := mocks.NewMockConnectionParametersManager(mockCtrl)
		mockCpm.EXPECT().TruncateConnectionID().Return(false).AnyTimes()

		cryptoStream = &stream{}

		streamsMap := newStreamsMap(nil, protocol.PerspectiveServer, nil)
		streamsMap.streams[1] = cryptoStream
		streamsMap.openStreams = []protocol.StreamID{1}
		streamFramer = newStreamFramer(streamsMap, nil)

		packer = &packetPacker{
			cryptoSetup:           &mockCryptoSetup{encLevelSeal: protocol.EncryptionForwardSecure},
			connectionParameters:  mockCpm,
			connectionID:          0x1337,
			packetNumberGenerator: newPacketNumberGenerator(protocol.SkipPacketAveragePeriodLength),
			streamFramer:          streamFramer,
			perspective:           protocol.PerspectiveServer,
		}
		publicHeaderLen = 1 + 8 + 2 // 1 flag byte, 8 connection ID, 2 packet number
		maxFrameSize = protocol.MaxFrameAndPublicHeaderSize - publicHeaderLen
		packer.version = protocol.VersionWhatever
	})

	It("returns nil when no packet is queued", func() {
		p, err := packer.PackPacket()
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("packs single packets", func() {
		f := &frames.StreamFrame{
			StreamID: 5,
			Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		b := &bytes.Buffer{}
		f.Write(b, 0)
		Expect(p.frames).To(HaveLen(1))
		Expect(p.raw).To(ContainSubstring(string(b.Bytes())))
	})

	It("stores the encryption level a packet was sealed with", func() {
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionForwardSecure
		f := &frames.StreamFrame{
			StreamID: 5,
			Data:     []byte("foobar"),
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p.encryptionLevel).To(Equal(protocol.EncryptionForwardSecure))
	})

	Context("diversificaton nonces", func() {
		var nonce []byte

		BeforeEach(func() {
			nonce = bytes.Repeat([]byte{'e'}, 32)
			packer.cryptoSetup.(*mockCryptoSetup).divNonce = nonce
		})

		It("doesn't include a div nonce, when sending a packet with initial encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionUnencrypted)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})

		It("includes a div nonce, when sending a packet with secure encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionSecure)
			Expect(ph.DiversificationNonce).To(Equal(nonce))
		})

		It("doesn't include a div nonce, when sending a packet with forward-secure encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionForwardSecure)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})

		It("doesn't send a div nonce as a client", func() {
			packer.perspective = protocol.PerspectiveClient
			ph := packer.getPublicHeader(protocol.EncryptionSecure)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})
	})

	It("packs a ConnectionClose", func() {
		ccf := frames.ConnectionCloseFrame{
			ErrorCode:    0x1337,
			ReasonPhrase: "foobar",
		}
		p, err := packer.PackConnectionClose(&ccf)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(&ccf))
	})

	It("doesn't send any other frames when sending a ConnectionClose", func() {
		ccf := frames.ConnectionCloseFrame{
			ErrorCode:    0x1337,
			ReasonPhrase: "foobar",
		}
		packer.controlFrames = []frames.Frame{&frames.WindowUpdateFrame{StreamID: 37}}
		streamFramer.AddFrameForRetransmission(&frames.StreamFrame{
			StreamID: 5,
			Data:     []byte("foobar"),
		})
		p, err := packer.PackConnectionClose(&ccf)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(&ccf))
	})

	It("packs only control frames", func() {
		packer.QueueControlFrame(&frames.RstStreamFrame{})
		packer.QueueControlFrame(&frames.WindowUpdateFrame{})
		p, err := packer.PackPacket()
		Expect(p).ToNot(BeNil())
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.raw).NotTo(BeEmpty())
	})

	It("increases the packet number", func() {
		packer.QueueControlFrame(&frames.RstStreamFrame{})
		p1, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p1).ToNot(BeNil())
		packer.QueueControlFrame(&frames.RstStreamFrame{})
		p2, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p2).ToNot(BeNil())
		Expect(p2.number).To(BeNumerically(">", p1.number))
	})

	It("packs a StopWaitingFrame first", func() {
		packer.packetNumberGenerator.next = 15
		swf := &frames.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(&frames.RstStreamFrame{})
		packer.QueueControlFrame(swf)
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.frames[0]).To(Equal(swf))
	})

	It("sets the LeastUnackedDelta length of a StopWaitingFrame", func() {
		packetNumber := protocol.PacketNumber(0xDECAFB) // will result in a 4 byte packet number
		packer.packetNumberGenerator.next = packetNumber
		swf := &frames.StopWaitingFrame{LeastUnacked: packetNumber - 0x100}
		packer.QueueControlFrame(&frames.RstStreamFrame{})
		packer.QueueControlFrame(swf)
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames[0].(*frames.StopWaitingFrame).PacketNumberLen).To(Equal(protocol.PacketNumberLen4))
	})

	It("does not pack a packet containing only a StopWaitingFrame", func() {
		swf := &frames.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(swf)
		p, err := packer.PackPacket()
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("packs a packet if it has queued control frames, but no new control frames", func() {
		packer.controlFrames = []frames.Frame{&frames.BlockedFrame{StreamID: 0}}
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
	})

	It("adds the version flag to the public header before the crypto handshake is finished", func() {
		packer.perspective = protocol.PerspectiveClient
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
		packer.controlFrames = []frames.Frame{&frames.BlockedFrame{StreamID: 0}}
		packer.connectionID = 0x1337
		packer.version = 123
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		hdr, err := ParsePublicHeader(bytes.NewReader(p.raw), protocol.PerspectiveClient, packer.version)
		Expect(err).ToNot(HaveOccurred())
		Expect(hdr.VersionFlag).To(BeTrue())
		Expect(hdr.VersionNumber).To(Equal(packer.version))
	})

	It("doesn't add the version flag to the public header for forward-secure packets", func() {
		packer.perspective = protocol.PerspectiveClient
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionForwardSecure
		packer.controlFrames = []frames.Frame{&frames.BlockedFrame{StreamID: 0}}
		packer.connectionID = 0x1337
		p, err := packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		hdr, err := ParsePublicHeader(bytes.NewReader(p.raw), protocol.PerspectiveClient, packer.version)
		Expect(err).ToNot(HaveOccurred())
		Expect(hdr.VersionFlag).To(BeFalse())
	})

	It("packs many control frames into 1 packets", func() {
		f := &frames.AckFrame{LargestAcked: 1}
		b := &bytes.Buffer{}
		f.Write(b, protocol.VersionWhatever)
		maxFramesPerPacket := int(protocol.MaxFrameAndPublicHeaderSize-publicHeaderLen) / b.Len()
		var controlFrames []frames.Frame
		for i := 0; i < maxFramesPerPacket; i++ {
			controlFrames = append(controlFrames, f)
		}
		packer.controlFrames = controlFrames
		payloadFrames, err := packer.composeNextPacket(maxFrameSize, false)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(maxFramesPerPacket))
		payloadFrames, err = packer.composeNextPacket(maxFrameSize, false)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(BeEmpty())
	})

	It("packs a lot of control frames into 2 packets if they don't fit into one", func() {
		blockedFrame := &frames.BlockedFrame{
			StreamID: 0x1337,
		}
		minLength, _ := blockedFrame.MinLength(0)
		maxFramesPerPacket := int(protocol.MaxFrameAndPublicHeaderSize-publicHeaderLen) / int(minLength)
		var controlFrames []frames.Frame
		for i := 0; i < maxFramesPerPacket+10; i++ {
			controlFrames = append(controlFrames, blockedFrame)
		}
		packer.controlFrames = controlFrames
		payloadFrames, err := packer.composeNextPacket(maxFrameSize, false)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(maxFramesPerPacket))
		payloadFrames, err = packer.composeNextPacket(maxFrameSize, false)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(10))
	})

	It("only increases the packet number when there is an actual packet to send", func() {
		packer.packetNumberGenerator.nextToSkip = 1000
		p, err := packer.PackPacket()
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
		Expect(packer.packetNumberGenerator.Peek()).To(Equal(protocol.PacketNumber(1)))
		f := &frames.StreamFrame{
			StreamID: 5,
			Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err = packer.PackPacket()
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.number).To(Equal(protocol.PacketNumber(1)))
		Expect(packer.packetNumberGenerator.Peek()).To(Equal(protocol.PacketNumber(2)))
	})

	Context("Stream Frame handling", func() {
		It("does not splits a stream frame with maximum size", func() {
			f := &frames.StreamFrame{
				Offset:         1,
				StreamID:       5,
				DataLenPresent: false,
			}
			minLength, _ := f.MinLength(0)
			maxStreamFrameDataLen := maxFrameSize - minLength
			f.Data = bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen))
			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(BeEmpty())
		})

		It("correctly handles a stream frame with one byte less than maximum size", func() {
			maxStreamFrameDataLen := protocol.MaxFrameAndPublicHeaderSize - publicHeaderLen - (1 + 1 + 2) - 1
			f1 := &frames.StreamFrame{
				StreamID: 5,
				Offset:   1,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)),
			}
			f2 := &frames.StreamFrame{
				StreamID: 5,
				Offset:   1,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize - 1)))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			p, err = packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
		})

		It("packs multiple small stream frames into single packet", func() {
			f1 := &frames.StreamFrame{
				StreamID: 5,
				Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
			}
			f2 := &frames.StreamFrame{
				StreamID: 5,
				Data:     []byte{0xBE, 0xEF, 0x13, 0x37},
			}
			f3 := &frames.StreamFrame{
				StreamID: 3,
				Data:     []byte{0xCA, 0xFE},
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			streamFramer.AddFrameForRetransmission(f3)
			p, err := packer.PackPacket()
			Expect(p).ToNot(BeNil())
			Expect(err).ToNot(HaveOccurred())
			b := &bytes.Buffer{}
			f1.Write(b, 0)
			f2.Write(b, 0)
			f3.Write(b, 0)
			Expect(p.frames).To(HaveLen(3))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[1].(*frames.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[2].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(p.raw).To(ContainSubstring(string(f1.Data)))
			Expect(p.raw).To(ContainSubstring(string(f2.Data)))
			Expect(p.raw).To(ContainSubstring(string(f3.Data)))
		})

		It("splits one stream frame larger than maximum size", func() {
			f := &frames.StreamFrame{
				StreamID: 7,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			maxStreamFrameDataLen := protocol.MaxFrameAndPublicHeaderSize - publicHeaderLen - minLength
			f.Data = bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+200)
			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(payloadFrames[0].(*frames.StreamFrame).Data).To(HaveLen(int(maxStreamFrameDataLen)))
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*frames.StreamFrame).Data).To(HaveLen(200))
			Expect(payloadFrames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(BeEmpty())
		})

		It("packs 2 stream frames that are too big for one packet correctly", func() {
			maxStreamFrameDataLen := protocol.MaxFrameAndPublicHeaderSize - publicHeaderLen - (1 + 1 + 2)
			f1 := &frames.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+100),
				Offset:   1,
			}
			f2 := &frames.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+100),
				Offset:   1,
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
			p, err = packer.PackPacket()
			Expect(p.frames).To(HaveLen(2))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[1].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(err).ToNot(HaveOccurred())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
			p, err = packer.PackPacket()
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(err).ToNot(HaveOccurred())
			Expect(p).ToNot(BeNil())
			p, err = packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("packs a packet that has the maximum packet size when given a large enough stream frame", func() {
			f := &frames.StreamFrame{
				StreamID: 5,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			f.Data = bytes.Repeat([]byte{'f'}, int(protocol.MaxFrameAndPublicHeaderSize-publicHeaderLen-minLength+1)) // + 1 since MinceLength is 1 bigger than the actual StreamFrame header
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p).ToNot(BeNil())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
		})

		It("splits a stream frame larger than the maximum size", func() {
			f := &frames.StreamFrame{
				StreamID: 5,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			f.Data = bytes.Repeat([]byte{'f'}, int(protocol.MaxFrameAndPublicHeaderSize-publicHeaderLen-minLength+2)) // + 2 since MinceLength is 1 bigger than the actual StreamFrame header

			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
		})

		It("refuses to send unencrypted stream data on a data stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionUnencrypted
			f := &frames.StreamFrame{
				StreamID: 3,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("sends non forward-secure data as the client", func() {
			packer.perspective = protocol.PerspectiveClient
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
			f := &frames.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			Expect(p.frames[0]).To(Equal(f))
		})

		It("does not send non forward-secure data as the server", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
			f := &frames.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("sends unencrypted stream data on the crypto stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSealCrypto = protocol.EncryptionUnencrypted
			cryptoStream.dataForWriting = []byte("foobar")
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionUnencrypted))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0]).To(Equal(&frames.StreamFrame{StreamID: 1, Data: []byte("foobar")}))
		})

		It("sends encrypted stream data on the crypto stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSealCrypto = protocol.EncryptionSecure
			cryptoStream.dataForWriting = []byte("foobar")
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0]).To(Equal(&frames.StreamFrame{StreamID: 1, Data: []byte("foobar")}))
		})

		It("does not pack stream frames if not allowed", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionUnencrypted
			packer.QueueControlFrame(&frames.AckFrame{})
			streamFramer.AddFrameForRetransmission(&frames.StreamFrame{StreamID: 3, Data: []byte("foobar")})
			p, err := packer.PackPacket()
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(func() { _ = p.frames[0].(*frames.AckFrame) }).NotTo(Panic())
		})
	})

	Context("Blocked frames", func() {
		It("queues a BLOCKED frame", func() {
			length := 100
			streamFramer.blockedFrameQueue = []*frames.BlockedFrame{{StreamID: 5}}
			f := &frames.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, length),
			}
			streamFramer.AddFrameForRetransmission(f)
			_, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(packer.controlFrames[0]).To(Equal(&frames.BlockedFrame{StreamID: 5}))
		})

		It("removes the dataLen attribute from the last StreamFrame, even if it queued a BLOCKED frame", func() {
			length := 100
			streamFramer.blockedFrameQueue = []*frames.BlockedFrame{{StreamID: 5}}
			f := &frames.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, length),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(HaveLen(1))
			Expect(p[0].(*frames.StreamFrame).DataLenPresent).To(BeFalse())
		})

		It("packs a connection-level BlockedFrame", func() {
			streamFramer.blockedFrameQueue = []*frames.BlockedFrame{{StreamID: 0}}
			f := &frames.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			_, err := packer.composeNextPacket(maxFrameSize, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(packer.controlFrames[0]).To(Equal(&frames.BlockedFrame{StreamID: 0}))
		})
	})

	It("returns nil if we only have a single STOP_WAITING", func() {
		packer.QueueControlFrame(&frames.StopWaitingFrame{})
		p, err := packer.PackPacket()
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(BeNil())
	})

	It("packs a single ACK", func() {
		ack := &frames.AckFrame{LargestAcked: 42}
		packer.QueueControlFrame(ack)
		p, err := packer.PackPacket()
		Expect(err).NotTo(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.frames[0]).To(Equal(ack))
	})

	It("does not return nil if we only have a single ACK but request it to be sent", func() {
		ack := &frames.AckFrame{}
		packer.QueueControlFrame(ack)
		p, err := packer.PackPacket()
		Expect(err).NotTo(HaveOccurred())
		Expect(p).ToNot(BeNil())
	})

	It("queues a control frame to be sent in the next packet", func() {
		wuf := &frames.WindowUpdateFrame{StreamID: 5}
		packer.QueueControlFrame(wuf)
		p, err := packer.PackPacket()
		Expect(err).NotTo(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(wuf))
	})

	Context("retransmitting of handshake packets", func() {
		swf := &frames.StopWaitingFrame{LeastUnacked: 1}
		sf := &frames.StreamFrame{
			StreamID: 1,
			Data:     []byte("foobar"),
		}

		BeforeEach(func() {
			packer.QueueControlFrame(swf)
		})

		It("packs a retransmission for a packet sent with no encryption", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionUnencrypted,
				Frames:          []frames.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(ContainElement(sf))
			Expect(p.frames).To(ContainElement(swf))
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionUnencrypted))
		})

		It("packs a retransmission for a packet sent with initial encryption", func() {
			nonce := bytes.Repeat([]byte{'e'}, 32)
			packer.cryptoSetup.(*mockCryptoSetup).divNonce = nonce
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames:          []frames.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(ContainElement(sf))
			Expect(p.frames).To(ContainElement(swf))
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			// a packet sent by the server with initial encryption contains the SHLO
			// it needs to have a diversification nonce
			Expect(p.raw).To(ContainSubstring(string(nonce)))
		})

		It("includes the diversification nonce on packets sent with initial encryption", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames:          []frames.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
		})

		// this should never happen, since non forward-secure packets are limited to a size smaller than MaxPacketSize, such that it is always possible to retransmit them without splitting the StreamFrame
		// (note that the retransmitted packet needs to have enough space for the StopWaitingFrame)
		It("refuses to send a packet larger than MaxPacketSize", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames: []frames.Frame{
					&frames.StreamFrame{
						StreamID: 1,
						Data:     bytes.Repeat([]byte{'f'}, int(protocol.MaxPacketSize-5)),
					},
				},
			}
			_, err := packer.PackHandshakeRetransmission(packet)
			Expect(err).To(MatchError("PacketPacker BUG: packet too large"))
		})

		It("refuses to retransmit packets that were sent with forward-secure encryption", func() {
			p := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionForwardSecure,
			}
			_, err := packer.PackHandshakeRetransmission(p)
			Expect(err).To(MatchError("PacketPacker BUG: forward-secure encrypted handshake packets don't need special treatment"))
		})

		It("refuses to retransmit packets without a StopWaitingFrame", func() {
			packer.stopWaiting = nil
			_, err := packer.PackHandshakeRetransmission(&ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
			})
			Expect(err).To(MatchError("PacketPacker BUG: Handshake retransmissions must contain a StopWaitingFrame"))
		})
	})

	Context("packing ACK packets", func() {
		It("packs ACK packets", func() {
			packer.QueueControlFrame(&frames.AckFrame{})
			p, err := packer.PackAckPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(p.frames).To(Equal([]frames.Frame{&frames.AckFrame{DelayTime: math.MaxInt64}}))
		})

		It("packs ACK packets with SWFs", func() {
			packer.QueueControlFrame(&frames.AckFrame{})
			packer.QueueControlFrame(&frames.StopWaitingFrame{})
			p, err := packer.PackAckPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(p.frames).To(Equal([]frames.Frame{
				&frames.AckFrame{DelayTime: math.MaxInt64},
				&frames.StopWaitingFrame{PacketNumber: 1, PacketNumberLen: 2},
			}))
		})
	})
})
