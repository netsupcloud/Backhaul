package transport

import (
	"encoding/binary"
	"io"
	"net"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"
	"github.com/sirupsen/logrus"
)

type LocalUDPConn struct {
	timeCreated int64
	payload     chan []byte
	remoteAddr  string
	listener    *net.UDPConn
	clientAddr  *net.UDPAddr
	IsClosed    bool // signal for closed
	IsCongested bool //or congested tcp connection
}

const BufferSize = 16 * 1024

func (s *TcpTransport) udpListener(localAddr string, remoteAddr string) {
	localUDPAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to resolve local address: %v", err)
	}

	listener, err := net.ListenUDP("udp", localUDPAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on local UDP port: %v", err)
	}

	defer listener.Close()

	s.logger.Infof("UDP listener started successfully, listening on address: %s", listener.LocalAddr().String())

	// Track active connections
	activeConnections := map[string]*LocalUDPConn{}

	// Buffer for UDP reads
	buf := make([]byte, BufferSize-2) // 2 bytes reserved for header

	// make a new channel for recieve udp packets
	udpChan := make(chan *LocalUDPConn, s.config.ChannelSize)

	// handle channel
	go s.handleUDPLoop(udpChan)

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				n, addr, err := listener.ReadFromUDP(buf)
				if err != nil {
					s.logger.Errorf("failed to read from UDP listener: %v", err)
					continue
				}

				// Create a unique identifier for the connection based on IP and port
				key := addr.String()

				// Check if the connection is already active
				if existingConn, exists := activeConnections[key]; exists {
					if existingConn.IsClosed || existingConn.IsCongested {
						if existingConn.IsClosed {
							s.logger.Debugf("connection with timestamp %d closed. Removing %s from active connections", existingConn.timeCreated, addr.String())
							close(existingConn.payload) // Close the payload channel
						} else if existingConn.IsCongested {
							s.logger.Debugf("connection with timestamp %d congested. Removing %s from active connections due to network congestion", existingConn.timeCreated, addr.String())
							// For congested connections, closing the payload channel immediately can cause abrupt TCP disconnection,
							// potentially leading to data loss. Instead, allow the connection to keep transferring data for 30 more
							// seconds (or until the payload channel becomes idle). The timer will close the TCP connection once it
							// times out. Further testing is needed to confirm this strategy's effect on overall performance and congestion handling.
						}
						
						delete(activeConnections, key)

					} else {
						// If it exists, send the payload to the existing connection's payload channel
						select {
						case existingConn.payload <- append([]byte(nil), buf[:n]...): // Copy the packet to avoid data overwriting
							s.logger.Tracef("buffered %d bytes for existing connection %s", n, addr.String())

						default:
							s.logger.Warnf("payload channel for connection %s is full, dropping udp packet", addr.String())
						}
						continue
					}
				}

				// Create a new payload channel for this connection,  Buffer up to 100,0000 packets for the connection
				// Generally affect the upload speed
				payloadChan := make(chan []byte, 100_000)

				// build the UDP packet
				newUDPConn := LocalUDPConn{
					timeCreated: time.Now().UnixNano(), // Just for debugging
					payload:     payloadChan,
					remoteAddr:  remoteAddr,
					listener:    listener,
					clientAddr:  addr,
					IsClosed:    false,
					IsCongested: false,
				}

				// store the connection info
				activeConnections[key] = &newUDPConn

				select {
				case udpChan <- &newUDPConn:
					s.logger.Debugf("accepted UDP connection from %s", addr.String())
					payloadChan <- append([]byte(nil), buf[:n]...) // send a copy of the new payload to the channel

					select {
					case s.reqNewConnChan <- struct{}{}: // Successfully requested a new tcp connection
					default: // The channel is full, do nothing
						s.logger.Warn("channel is full, cannot request a new connection")
					}

				default:
					s.logger.Warn("UDP channel is full, dropping packet.")
				}
			}
		}
	}()

	<-s.ctx.Done()
}

func (s *TcpTransport) handleUDPLoop(udpChan chan *LocalUDPConn) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case localConn := <-udpChan:
		loop:
			for {
				select {
				case <-s.ctx.Done():
					return

				case tunnelConn := <-s.tunnelChannel:
					// Send the target addr over the connection
					if err := utils.SendBinaryString(tunnelConn, localConn.remoteAddr, utils.SG_UDP); err != nil {
						s.logger.Errorf("%v", err)
						tunnelConn.Close()
						continue loop
					}

					// Handle data exchange between connections
					go UDPConnectionHandler(localConn, tunnelConn, s.logger, s.usageMonitor, localConn.listener.LocalAddr().(*net.UDPAddr).Port, s.config.Sniffer)

					s.logger.Debugf("initiate new handler for connection %s with timestamp %d", localConn.clientAddr.String(), localConn.timeCreated)
					break loop
				}
			}
		}
	}
}

func UDPConnectionHandler(udp *LocalUDPConn, tcp net.Conn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	done := make(chan struct{})

	go func() {
		udpToTCP(tcp, udp, logger, usage, remotePort, sniffer)
		done <- struct{}{}
	}()

	var rtt int64 = 100 // time.Millisecond, for tcp congest control

	tcpToUDP(tcp, udp, logger, usage, remotePort, sniffer, rtt)

	<-done
}

func udpToTCP(tcp net.Conn, udp *LocalUDPConn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	// Create a header (2 bytes) to hold the size of the data
	header := make([]byte, 2)

	defer func() {
		udp.IsClosed = true
		tcp.Close()
	}()

	inactivityTimeout := 10 * time.Second // Define a 30-second inactivity timeout

	for {
		select {
		case data, ok := <-udp.payload: // Wait for data on the UDP payload channel
			if !ok {
				return
			}

			packetSize := len(data) // Calculate the packet size (data length)

			// the listener buffer size is 16KB, just for preventing bugs in the future!
			if packetSize > 65535 { // Check for overflow, since 2 bytes can only store values up to 65535 ~ 64KB
				logger.Errorf("packet too large to send, size: %d bytes", packetSize)
				continue
			}

			binary.BigEndian.PutUint16(header, uint16(packetSize)) // Store the packet size at 2 bytes

			// Prepend the header to the data
			packet := append(header, data...)

			totalWritten := 0
			for totalWritten < len(packet) { // Use the total packet length (header + data)
				w, err := tcp.Write(packet[totalWritten:])
				if err != nil {
					logger.Errorf("failed to write UDP payload to TCP: %v", err)
					return
				}
				totalWritten += w
			}

			logger.Tracef("received %d bytes, forwarded %d bytes from UDP to TCP", packetSize, totalWritten-2)

			if sniffer {
				usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
			}

		case <-time.After(inactivityTimeout): // Timeout after 30 seconds of inactivity
			logger.Debugf("connection with timestamp %d and address %s idle for 30 seconds, closing", udp.timeCreated, udp.clientAddr.String())
			return
		}
	}
}

func tcpToUDP(tcp net.Conn, udp *LocalUDPConn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool, rtt int64) {
	buf := make([]byte, BufferSize)
	lenBuf := make([]byte, 2)       // Buffer to store the 2-byte packet length
	timestampBuf := make([]byte, 8) // Buffer for timestamp (8 bytes)

	defer func() {
		udp.IsClosed = true
		tcp.Close()
	}()

	for {
		// First, read the 8-byte timestamp from the packet
		_, err := io.ReadFull(tcp, timestampBuf)
		if err != nil {
			if err == io.EOF {
				logger.Debug("TCP connection closed.")
			} else {
				logger.Debugf("failed to read timestamp from TCP connection: %v", err)
			}
			return
		}

		// Convert the 8-byte timestamp header into a time.Time object
		packetTimestamp := time.UnixMilli(int64(binary.BigEndian.Uint64(timestampBuf)))

		// Get the current time and calculate the time difference
		currentTime := time.Now()
		packetAge := currentTime.Sub(packetTimestamp)

		// If the packet age exceeds the threshold (2-3x RTT), flag the connection as congested
		if packetAge.Milliseconds() > 3*rtt {
			udp.IsCongested = true
		}

		// Read the 2-byte packet length header from the TCP connection
		_, err = io.ReadFull(tcp, lenBuf)
		if err != nil {
			if err == io.EOF {
				logger.Debug("TCP connection closed.")
			} else {
				logger.Errorf("failed to read packet length from TCP connection: %v", err)
			}
			return
		}

		// Convert the 2-byte length header into an integer
		packetSize := int(binary.BigEndian.Uint16(lenBuf))

		// Check if the packet size is valid
		if packetSize > len(buf) {
			logger.Errorf("packet size exceeds buffer size: %d bytes", packetSize)
			return
		}

		// Now use io.ReadFull to read the actual packet data from TCP based on the packetSize
		_, err = io.ReadFull(tcp, buf[:packetSize])
		if err != nil {
			if err == io.EOF {
				logger.Debug("TCP connection closed.")
			} else {
				logger.Errorf("failed to read from TCP connection: %v", err)
			}
			return
		}

		// Forward the data to the UDP client address
		if udp.clientAddr != nil {
			totalWritten := 0
			for totalWritten < packetSize {
				w, err := udp.listener.WriteToUDP(buf[totalWritten:packetSize], udp.clientAddr)
				if err != nil {
					logger.Errorf("failed to forward TCP response to UDP client: %v", err)
					return
				}

				totalWritten += w
			}

			if sniffer {
				usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
			}

			logger.Tracef("read %d bytes from TCP, forwarded %d bytes to UDP", packetSize, totalWritten)
		}
	}
}