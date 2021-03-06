/*
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * Author: Justin Wong <justin.w.xd@gmail.com>
 *
 */

package hop

import (
    "github.com/bigeagle/water"
    "github.com/bigeagle/water/waterutil"
    "encoding/binary"
    "bytes"
    "fmt"
    "net"
    "os"
    "os/signal"
    "syscall"
    "time"
    "sync/atomic"
    "sync"
)

// a udpPacket
type udpPacket struct {
    // client's addr
    addr *net.UDPAddr
    // data
    data []byte
    // channel
    channel int
}

type HopServer struct {
    // config
    cfg HopServerConfig
    // interface
    iface *water.Interface
    // subnet
    ipnet *net.IPNet
    // IP Pool
    ippool *hopIPPool
    // client peers, key is the mac address, value is a HopPeer record
    peers map[uint64]*HopPeer

    // channel to put in packets read from udpsocket
    fromNet chan *udpPacket
    // channel to put packets to send through udpsocket
    toNet []chan *udpPacket
    // channel to put frames read from tun/tap device
    fromIface chan []byte
    toIface chan *HopPacket
    _lock sync.RWMutex
    _chanBufSize int
}


func NewServer(cfg HopServerConfig) error {
    var err error
    logger.Debug("%v", cfg)

    cipher, err = newHopCipher([]byte(cfg.Key))
    if err != nil {
        logger.Debug("hi")
        return err
    }

    if cfg.MTU != 0 {
        MTU = cfg.MTU
    }


    hopServer := new(HopServer)
    hopServer._chanBufSize = 256
    hopServer.fromNet = make(chan *udpPacket, hopServer._chanBufSize)
    hopServer.fromIface = make(chan []byte, hopServer._chanBufSize)
    hopServer.toIface = make(chan *HopPacket, hopServer._chanBufSize)
    hopServer.peers = make(map[uint64]*HopPeer)
    hopServer.cfg = cfg
    hopServer.toNet = make([]chan *udpPacket, (cfg.HopEnd-cfg.HopStart+1))
    hopServer.ippool = new(hopIPPool)

    iface, err := newTun("")
    if err != nil {
        logger.Debug("hi")
        return err
    }
    hopServer.iface = iface
    ip, subnet, err := net.ParseCIDR(cfg.Addr)
    err = setTunIP(iface, ip, subnet)
    if err != nil {
        logger.Debug("hi")
        return err
    }
    hopServer.ipnet = &net.IPNet{ip, subnet.Mask}
    hopServer.ippool.subnet = subnet

    if cfg.FixMSS {
        fixMSS(iface.Name(), true)
    }

    // traffic morpher
    switch cfg.MorphMethod {
    case "randsize":
        m := newRandMorpher(MTU)
        hopFrager = newHopFragmenter(m)
        logger.Info("Using RandomSize Morpher")
    default:
        logger.Info("No Traffic Morphing")
    }


    // forward device frames to socket and socket packets to device
    go hopServer.forwardFrames()

    // go func() {
    //     defer hopServer.cleanUp()
    //     redirectPort(cfg.HopRange, cfg.Port)
    // }()
    go hopServer.cleanUp()

    // serve for multiple ports
    for idx, port := 0, cfg.HopStart; port <= cfg.HopEnd; port++ {
        go hopServer.listenAndServe(cfg.ListenAddr, fmt.Sprintf("%d", port), idx)
        idx++
    }

    logger.Debug("Recieving iface frames")


    // handle interface

    go func() {
        for {
            hp := <-hopServer.toIface
            logger.Debug("New Net packet to device")
            _, err := iface.Write(hp.payload)
            //logger.Debug("n: %d, len: %d", n, len(hp.payload))
            if err != nil {
                logger.Error(err.Error())
                return
            }
        }
    }()

    buf := make([]byte, IFACE_BUFSIZE)
    for {
        n, err := iface.Read(buf)
        if err != nil {
            return err
        }

        hpbuf := make([]byte, n+HOP_HDR_LEN)
        copy(hpbuf[HOP_HDR_LEN:], buf[:n])
        hopServer.fromIface <- hpbuf
    }

}

func (srv *HopServer) listenAndServe(addr string, port string, idx int) {
    port = addr + ":" + port
    udpAddr, err := net.ResolveUDPAddr("udp", port)
    if err != nil {
        logger.Error("Invalid port: %s", port)
        return
    }
    udpConn, err := net.ListenUDP("udp", udpAddr)
    if err != nil {
        logger.Error("Failed to listen udp port %s: %s", port, err.Error())
        return
    }

    toNet := make(chan *udpPacket, srv._chanBufSize)

    go func() {
        defer srv._lock.Unlock()
        srv._lock.Lock()
        srv.toNet[idx] = toNet
        // logger.Debug("Listening on port %s", port)
    }()

    go func() {
        for {
            packet := <-toNet
            // logger.Debug("index: %d, port: %s", idx, port)
            // logger.Debug("client addr: %v", packet.addr)
            udpConn.WriteTo(packet.data, packet.addr)
        }
    }()

    for {
        var plen int
        packet := new(udpPacket)
        packet.channel = idx
        buf := make([]byte, IFACE_BUFSIZE)
        // logger.Debug("Recieving packet %s", port)
        plen, packet.addr, err = udpConn.ReadFromUDP(buf)
        // logger.Debug("New UDP Packet from: %v", packet.addr)

        packet.data = buf[:plen]
        if err != nil {
            logger.Error(err.Error())
            return
        }

        srv.fromNet <- packet
    }

}

func (srv *HopServer) forwardFrames() {

    // packet map
    pktHandle := map[byte](func(*udpPacket, *HopPacket)){
        HOP_FLG_PSH: srv.handleKnock,
        HOP_FLG_HSH: srv.handleHandshake,
        HOP_FLG_HSH|HOP_FLG_ACK: srv.handleHandshakeAck,
        HOP_FLG_DAT: srv.handleDataPacket,
        HOP_FLG_DAT|HOP_FLG_MFR: srv.handleDataPacket,
        HOP_FLG_FIN: srv.handleFinish,
    }

    for {
        select {
        case pack := <-srv.fromIface:
            logger.Debug("New iface Frame")
            // first byte is left for opcode
            frame := pack[HOP_HDR_LEN:]
            dest := waterutil.IPv4Destination(frame).To4()
            mkey := ip4_uint64(dest)

            logger.Debug("ip dest: %v", dest)
            if hpeer, found := srv.peers[mkey]; found {
                srv.bufferToClient(hpeer, pack)
            } else {
                logger.Warning("client peer with key %d not found", mkey)
            }

        case packet := <-srv.fromNet:

            hPack, err := unpackHopPacket(packet.data)
            if err == nil {
                logger.Debug("New UDP Packet from: %v", packet.addr)

                if handle_func, ok := pktHandle[hPack.Flag]; ok {
                    handle_func(packet, hPack)
                } else {
                    logger.Error("Unkown flag: %x", hPack.Flag)
                }
            } else {
                logger.Error(err.Error())
            }
        }

    }
}

func (srv *HopServer) toClient(peer *HopPeer, flag byte, payload []byte, noise bool) {
    hp := new(HopPacket)
    hp.Seq = peer.Seq()
    hp.Flag = flag
    hp.payload = payload

    if addr, idx, ok := peer.addr(); ok {
        logger.Debug("peer: %v", addr)
        upacket := &udpPacket{addr, hp.Pack(), idx}
        srv.toNet[idx] <- upacket
    } else {
        logger.Debug("peer not found")
    }
}

func (srv *HopServer) bufferToClient(peer *HopPeer, buf []byte) {
    if hopFrager == nil {
        // if no traffic morphing
        hp := new(HopPacket)
        hp.Flag = HOP_FLG_DAT
        hp.buf = buf
        hp.payload = buf[HOP_HDR_LEN:]
        hp.Seq = peer.Seq()

        if addr, idx, ok := peer.addr(); ok {
            upacket := &udpPacket{addr, hp.Pack(), idx}
            srv.toNet[idx] <- upacket
        }
    } else {
        // with traffic morphing
        frame := buf[HOP_HDR_LEN:]
        packets := hopFrager.Fragmentate(peer, frame)
        for _, hp := range(packets) {
            if addr, idx, ok := peer.addr(); ok {
                upacket := &udpPacket{addr, hp.Pack(), idx}
                srv.toNet[idx] <- upacket
            }
        }
    }
}

func (srv *HopServer) handleKnock(u *udpPacket, hp *HopPacket) {
    logger.Debug("Handle Knock")
    sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
    logger.Debug("port knock from client %v, sid: %d", u.addr, sid)
    sid = (sid << 32) & uint64(0xFFFFFFFF00000000)

    hpeer, ok := srv.peers[sid]
    if ! ok {
        hpeer = newHopPeer(sid, srv, u.addr, u.channel)
        srv.peers[sid] = hpeer
    } else {
        hpeer.insertAddr(u.addr, u.channel)
    }

}

func (srv *HopServer) handleHandshake(u *udpPacket, hp *HopPacket) {
    logger.Debug("Handle Handshake")
    sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
    sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
    logger.Debug("handshake from client %v, sid: %d", u.addr, sid)

    hpeer, ok := srv.peers[sid]
    if ! ok {
        hpeer = newHopPeer(sid, srv, u.addr, u.channel)
        srv.peers[sid] = hpeer
    } else {
        hpeer.insertAddr(u.addr, u.channel)
    }

    cltIP, err := srv.ippool.next()
    if err != nil {
        msg := fmt.Sprintf("%s", err.Error())
        srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_FIN, []byte(msg), true)
        delete(srv.peers, sid)
    } else {
        hpeer.ip = cltIP.IP.To4()
        buf := bytes.NewBuffer(make([]byte, 0, 8))
        buf.Write([]byte(hpeer.ip))
        buf.Write([]byte(cltIP.Mask))
        key := ip4_uint64(hpeer.ip)

        logger.Debug("assign address %s, route key %d", cltIP, key)
        srv.peers[key] = hpeer
        atomic.StoreInt32(&hpeer.state, HOP_STAT_HANDSHAKE)
        srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_ACK, buf.Bytes(), true)
        hpeer.hsDone = make(chan byte)
        go func(){
            for i := 0; i < 5; i++ {
                select {
                case <- hpeer.hsDone:
                    hpeer.state = HOP_STAT_WORKING
                    return
                case <- time.After(2 * time.Second):
                    logger.Debug("Client Handshake Timeout")
                    srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_ACK, buf.Bytes(), true)
                }
            }
            // timeout,  kick
            srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_FIN, []byte{}, true)
            srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_FIN, []byte{}, true)
            srv.toClient(hpeer, HOP_FLG_HSH | HOP_FLG_FIN, []byte{}, true)

            srv.ippool.relase(hpeer.ip)
            delete(srv.peers, sid)
            delete(srv.peers, key)

        }()
    }

}

func (srv *HopServer) handleHandshakeAck(u *udpPacket, hp *HopPacket) {
    logger.Debug("Handle HandshakeAck")
    sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
    sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
    hpeer, ok := srv.peers[sid]
    if ! ok {
        return
    }
    logger.Debug("Client Handshake Done")
    logger.Info("Client %d Connected", sid)
    atomic.StoreInt32(&hpeer.state, HOP_STAT_WORKING)
    hpeer.hsDone <- 1
}

func (srv *HopServer) handleDataPacket(u *udpPacket, hp *HopPacket) {
    logger.Debug("Handle dataPacket")
    sid := uint64(hp.Sid)
    sid = (sid << 32) & uint64(0xFFFFFFFF00000000)

    if peer, ok := srv.peers[sid]; ok {
        // logger.Debug("n peer addrs: %v", len(peer._addrs_lst))
        // peer.insertAddr(u.addr, u.channel)
        peer.recvBuffer.Push(hp)
    }
}

func (srv *HopServer) handleFinish(u *udpPacket, hp *HopPacket) {
    logger.Debug("Handle Finish")
    sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
    sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
    logger.Info("releasing client %v, sid: %d", u.addr, sid)

    hpeer, ok := srv.peers[sid]
    if ! ok {
        return
    }

    key := ip4_uint64(hpeer.ip)
    srv.ippool.relase(hpeer.ip)
    delete(srv.peers, sid)
    delete(srv.peers, key)
    srv.toClient(hpeer, HOP_FLG_FIN | HOP_FLG_ACK, []byte{}, false)
}

func (srv *HopServer) cleanUp() {
    c := make(chan os.Signal, 1)
    signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
    <-c

    clearMSS(srv.iface.Name(), true)
    os.Exit(0)
}
