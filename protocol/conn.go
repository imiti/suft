package suft

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

const (
	MAX_RETRIES = 6
	MIN_RTT     = 8
	MIN_RTO     = 30
	MIN_ATO     = 2
	MAX_ATO     = 30
)

const (
	VACK_SCHED = iota + 1
	VACK_QUICK
	VACK_MUST
	VSWND_ACTIVE
	VRETR_IMMED
)

const (
	RETR_REST = -1
	_CLOSE    = 0xff
)

var debug int
var bandwidth int64
var fastRetransmitEnabled bool
var superRetransmit bool

func nodeOf(pk *packet) *qNode {
	return &qNode{packet: pk}
}

func (c *Conn) internalRecvLoop() {
	defer func() {
		// avoid send to closed channel while some replaying
		// data packets were received in shutting down.
		_ = recover()
	}()
	var buf, body []byte
	for {
		select {
		case buf = <-c.evRecv:
			if buf != nil {
				body = buf[TH_SIZE:]
			} else { // shutdown
				return
			}
		}
		pk := new(packet)
		// keep the original buffer, so we could recycle it in future
		pk.buffer = buf
		unmarshall(pk, body)
		if pk.flag&F_SACK != 0 {
			c.processSAck(pk)
			continue
		}
		if pk.flag&F_ACK != 0 {
			c.processAck(pk)
		}
		if pk.flag&F_DATA != 0 {
			c.insertData(pk)
		} else if pk.flag&F_FIN != 0 {
			go c.closeR(pk)
		}
	}
}

func (c *Conn) internalSendLoop() {
	var timer = NewTimer(c.rtt)
	for {
		select {
		case v := <-c.evSWnd:
			switch v {
			case VRETR_IMMED:
				c.outlock.Lock()
				c.retransmit2()
				c.outlock.Unlock()
			case VSWND_ACTIVE:
				timer.TryActive(c.rtt)
			case _CLOSE:
				return
			}
		case <-timer.C: // timeout yet
			var notifySender bool
			c.outlock.Lock()
			rest, _ := c.retransmit()
			switch rest {
			case RETR_REST, 0: // nothing to send
				if c.outQ.size() > 0 {
					timer.Reset(c.rtt)
				} else {
					timer.Stop()
					// avoid sender blocking
					notifySender = true
				}
			default: // recent rto point
				timer.Reset(minI64(rest, c.rtt))
			}
			c.outlock.Unlock()
			if notifySender {
				select {
				case c.evSend <- 1:
				default:
				}
			}
		}
	}
}

func (c *Conn) internalAckLoop() {
	var ackTimer = NewTimer(c.ato)
	var lastAckState byte
	for {
		var v byte
		select {
		case <-ackTimer.C:
			v = VACK_MUST
		case v = <-c.evAck:
			ackTimer.TryActive(c.ato)
			if lastAckState != v {
				v = VACK_MUST
			} else if v == _CLOSE {
				return
			}
			lastAckState = v
		}
		c.inlock.Lock()
		if pkAck := c.makeAck(v); pkAck != nil {
			c.internalWrite(nodeOf(pkAck))
		}
		c.inlock.Unlock()
	}
}

func (c *Conn) retransmit() (rest int64, count int32) {
	var now, rto = Now(), c.rto
	var limit = c.cwnd
	for item := c.outQ.head; item != nil && limit > 0; item = item.next {
		if item.scnt != -1 { // ACKed has scnt==-1
			diff := now - item.sent
			if diff > rto { // already rto
				c.internalWrite(item)
				count++
			} else {
				// continue search next min rto duration
				if rest > 0 {
					rest = minI64(rest, rto-diff+1)
				} else {
					rest = rto - diff + 1
				}
				limit--
			}
		}
	}
	c.outDupCnt += int(count)
	if count > 0 {
		// shrink cwnd to 1/2 if RTO 1/8 cwnd in FR or if RTO 1/4 cwnd in non-FR
		shrcond := (fastRetransmitEnabled && count > c.cwnd>>3) || (!fastRetransmitEnabled && count > c.cwnd>>2)
		if shrcond && !superRetransmit {
			log.Printf("shrink cwnd from=%d to=%d s/2=%d", c.cwnd, c.cwnd>>1, c.swnd>>1)
			c.lastShrink = Now()
			// ensure cwnd >= swnd/2
			c.cwnd = maxI32(c.cwnd>>1, c.swnd>>1)
		}
	}
	if c.outQ.size() > 0 {
		return
	}
	return RETR_REST, 0
}

func (c *Conn) retransmit2() (count int32) {
	var fRtt = c.rtt + maxI64(c.rtt>>4, 1)
	var limit, now = maxI32(minI32(c.cwnd-c.outPending, c.cwnd>>2), 8), Now()
	for item := c.outQ.head; item != nil && count < limit; item = item.next {
		if item.scnt != -1 { // ACKed has scnt==-1
			if item.miss >= 3 && now-item.sent >= fRtt {
				//item.miss = 0
				c.internalWrite(item)
				c.fRCnt++
				count++
			}
		}
	}
	c.outDupCnt += int(count)
	return
}

func (c *Conn) inputAndSend(pk *packet) error {
	item := &qNode{packet: pk}
	c.outlock.Lock()
	// inflight packets exceeds cwnd
	// inflight includes: 1, unacked; 2, missed
	for c.outPending >= c.cwnd {
		c.outlock.Unlock()
		if c.wtmo > 0 {
			var tmo int64
			tmo, c.wtmo = c.wtmo, 0
			select {
			case v := <-c.evSend:
				if v == _CLOSE {
					return io.EOF
				}
			case <-NewTimerChan(tmo):
				return ErrIOTimeout
			}
		} else {
			if v := <-c.evSend; v == _CLOSE {
				return io.EOF
			}
		}
		c.outlock.Lock()
	}
	c.outPending++
	c.outPkCnt++
	c.mySeq++
	pk.seq = c.mySeq
	c.outQ.appendTail(item)
	c.internalWrite(item)
	c.outlock.Unlock()
	// active resending timer
	// must blocking
	c.evSWnd <- VSWND_ACTIVE
	return nil
}

func (c *Conn) internalWrite(item *qNode) {
	// TODO Is there a better way to handle exceptions?
	if item.scnt >= 20 {
		if item.flag&F_FIN != 0 {
			c.fakeShutdown()
			c.dest = nil
			return
		} else {
			log.Panicln("too many retry............", item)
		}
	}
	// update current sent time and prev sent time
	item.sent, item.sent_1 = Now(), item.sent
	item.scnt++
	buf := item.marshall(c.connId)
	if debug >= 3 {
		var pkType = packetTypeNames[item.flag]
		if item.flag&F_SACK != 0 {
			log.Printf("send %s trp=%d on=%d %x", pkType, item.seq, item.ack, buf[AH_SIZE+4:])
		} else {
			log.Printf("send %s seq=%d ack=%d scnt=%d len=%d", pkType, item.seq, item.ack, item.scnt, len(buf)-TH_SIZE)
		}
	}
	c.sock.WriteToUDP(buf, c.dest)
}

func (c *Conn) logAck(ack uint32) {
	c.lastAck = ack
	c.lastAckTime = Now()
}

func (c *Conn) makeLastAck() (pk *packet) {
	c.inlock.Lock()
	defer c.inlock.Unlock()
	if Now()-c.lastAckTime < c.rtt {
		return nil
	}
	pk = &packet{
		ack:  maxU32(c.lastAck, c.inMaxCtnSeq),
		flag: F_ACK,
	}
	c.logAck(pk.ack)
	return
}

func (c *Conn) makeAck(demand byte) (pk *packet) {
	if demand < VACK_MUST && Now()-c.lastAckTime < c.ato {
		return
	}
	//	    ready Q <-|
	//	              |-> outQ start (or more right)
	//	              |-> bitmap start
	//	[predecessor]  [predecessor+1]  [predecessor+2] .....
	var fakeSAck bool
	var predecessor = c.inMaxCtnSeq
	bmap, tbl := c.inQ.makeHolesBitmap(predecessor)
	if len(bmap) <= 0 { // fake sack
		bmap = make([]uint64, 1)
		bmap[0], tbl = 1, 1
		fakeSAck = true
	}
	// head 4-byte: TBL:1 | SCNT:1 | DELAY:2
	buf := make([]byte, len(bmap)*8+4)
	pk = &packet{
		ack:     predecessor + 1,
		flag:    F_SACK,
		payload: buf,
	}
	if fakeSAck {
		pk.ack--
	}
	buf[0] = byte(tbl)
	// mark delayed time according to the time reference point
	if trp := c.inQ.lastIns; trp != nil {
		delayed := Now() - trp.sent
		if delayed < c.rtt {
			pk.seq = trp.seq
			pk.flag |= F_TIME
			buf[1] = byte(trp.scnt)
			if delayed <= 0 {
				delayed = 1
			}
			binary.BigEndian.PutUint16(buf[2:], uint16(delayed))
		}
	}
	buf1 := buf[4:]
	for i, b := range bmap {
		binary.BigEndian.PutUint64(buf1[i*8:], b)
	}
	c.logAck(predecessor)
	return
}

func unmarshallSAck(data []byte) (bmap []uint64, tbl uint32, delayed uint16, scnt int8) {
	if len(data) > 0 {
		bmap = make([]uint64, len(data)>>3)
	} else {
		return
	}
	tbl = uint32(data[0])
	scnt = int8(data[1])
	delayed = binary.BigEndian.Uint16(data[2:])
	data = data[4:]
	for i := 0; i < len(bmap); i++ {
		bmap[i] = binary.BigEndian.Uint64(data[i*8:])
	}
	return
}

func calSwnd(rtt int64) int32 {
	w := int32(bandwidth * rtt / (8000 * MSS))
	if w <= 640 {
		return w
	} else {
		return 640
	}
}

func (c *Conn) measure(seq uint32, delayed int64, scnt int8) {
	target := c.outQ.get(seq)
	if target != nil {
		var lastSent int64
		switch target.scnt - scnt {
		case 0:
			// not sent again since this ack was sent out
			lastSent = target.sent
		case 1:
			// sent again once since this ack was sent out
			// then use prev sent time
			lastSent = target.sent_1
		default:
			// can't measure here because the packet was sent too many times
			return
		}
		// real-time rtt
		rtt := Now() - lastSent - delayed
		// reject these abnormal measures:
		// 1. rtt too small -> rtt/8
		// 2. backlogging too long
		if rtt < maxI64(c.rtt>>3, 1) || delayed > c.rtt>>1 {
			return
		}
		err := rtt - (c.srtt >> 3)
		// 1/8 new + 7/8 old
		c.srtt += err
		c.rtt = c.srtt >> 3
		if c.rtt < MIN_RTT {
			c.rtt = MIN_RTT
		}
		// s-swnd 1/8
		swnd := c.swnd<<3 - c.swnd + calSwnd(c.rtt)
		c.swnd = swnd >> 3
		c.ato = c.rtt >> 4
		if c.ato < MIN_ATO {
			c.ato = MIN_ATO
		}
		if err < 0 {
			err = -err
			err -= c.mdev >> 2
			if err > 0 {
				err >>= 3
			}
		} else {
			err -= c.mdev >> 2
		}
		// mdev = 3/4 mdev + 1/4 new
		c.mdev += err
		rto := c.rtt + maxI64(c.rtt<<1, c.mdev)
		if rto >= c.rto {
			c.rto = rto
		} else {
			c.rto = (c.rto + rto) >> 1
		}
		if c.rto < MIN_RTO {
			c.rto = MIN_RTO
		}
		if debug >= 1 {
			log.Printf("--- rtt=%d srtt=%d rto=%d swnd=%d", c.rtt, c.srtt, c.rto, c.swnd)
		}
	}
}

func (c *Conn) processSAck(pk *packet) {
	c.outlock.Lock()
	bmap, tbl, delayed, scnt := unmarshallSAck(pk.payload)
	if bmap == nil { // bad packet
		return
	}
	if pk.flag&F_TIME != 0 {
		c.measure(pk.seq, int64(delayed), scnt)
	}
	deleted, missed, continuous := c.outQ.deleteByBitmap(bmap, pk.ack, tbl)
	if fastRetransmitEnabled && !continuous {
		// peer Q is uncontinuous, then trigger FR
		select {
		case c.evSWnd <- VRETR_IMMED:
		default:
		}
	}
	if deleted > 0 {
		c.ackHit(deleted, missed)
		// lock is released
	} else {
		c.outlock.Unlock()
	}
	if debug >= 2 {
		log.Printf("SACK qhead=%d deleted=%d outPending=%d on=%d %016x",
			c.outQ.distanceOfHead(0), deleted, c.outPending, pk.ack, bmap)
	}
}

func (c *Conn) processAck(pk *packet) {
	c.outlock.Lock()
	if end := c.outQ.get(pk.ack); end != nil { // ack hit
		_, deleted := c.outQ.deleteBefore(end)
		c.ackHit(deleted, 0) // lock is released
		if debug >= 2 {
			log.Printf("ACK hit on=%d", pk.ack)
		}
		// special case: ack the FIN
		if pk.seq == _FIN_ACK_SEQ {
			select {
			case c.evClose <- S_FIN0:
			default:
			}
		}
	} else { // duplicated ack
		if debug >= 2 {
			log.Printf("ACK miss on=%d", pk.ack)
		}
		if pk.flag&F_SYN != 0 { // No.3 Ack lost
			if pkAck := c.makeLastAck(); pkAck != nil {
				c.internalWrite(nodeOf(pkAck))
			}
		}
		c.outlock.Unlock()
	}
}

func (c *Conn) ackHit(deleted, missed int32) {
	// must in outlock
	c.outPending -= deleted
	if c.cwnd < c.swnd && Now()-c.lastShrink > c.rtt {
		c.cwnd += c.cwnd >> 1
	}
	if c.cwnd > c.swnd {
		c.cwnd = c.swnd
	}
	if missed >= c.missed {
		c.missed = missed
	} else {
		c.missed = (c.missed + missed) >> 1
	}
	c.cwnd += c.missed
	c.outlock.Unlock()
	select {
	case c.evSend <- 1:
	default:
	}
}

func (c *Conn) insertData(pk *packet) {
	c.inlock.Lock()
	defer c.inlock.Unlock()
	exists := c.inQ.contains(pk.seq)
	// duplicated with queued or already ACKed
	// means: last ACK were lost
	if exists || pk.seq <= c.inMaxCtnSeq {
		// then send ACK for dups
		select {
		case c.evAck <- VACK_MUST:
		default:
		}
		if debug >= 2 {
			dumpQ(fmt.Sprint("duplicated ", pk.seq), c.inQ)
		}
		c.inDupCnt++
		return
	}
	// record current time in sent and regard as received time
	item := &qNode{packet: pk, sent: Now()}
	dis := c.inQ.searchInsert(item, c.lastReadSeq)
	if debug >= 3 {
		log.Printf("\t\t\trecv DATA seq=%d dis=%d lastReadSeq=%d", item.seq, dis, c.lastReadSeq)
	}
	var ackState byte = VACK_MUST
	var available bool
	switch dis {
	case 0:
		// duplicated with history
		c.inDupCnt++
		return
	case 1:
		if c.inQDirty {
			max, continued := c.inQ.searchMaxContinued(c.lastReadSeq + 1)
			if continued {
				c.inMaxCtnSeq, available = max.seq, true
				// whole Q is ordered
				if max == c.inQ.tail {
					c.inQDirty = false
				}
			} // else: those holes still exists.
		} else {
			// here is an ideal situation
			c.inMaxCtnSeq, available = pk.seq, true
			ackState = VACK_QUICK
		}

	default: // there is an unordered packet, hole occurred here.
		if !c.inQDirty {
			c.inQDirty = true
		}
	}
	// write valid received count
	c.inPkCnt++
	c.inQ.lastIns = item
	select {
	case c.evAck <- ackState:
	default:
	}
	if available { // notify reader
		select {
		case c.evRead <- 1:
		default:
		}
	}
}

func (c *Conn) readInQ() bool {
	c.inlock.Lock()
	defer c.inlock.Unlock()
	// read already <-|-> expected Q
	//  [lastReadSeq] | [lastReadSeq+1] [lastReadSeq+2] ......
	if c.inQ.isEqualsHead(c.lastReadSeq+1) && c.lastReadSeq < c.inMaxCtnSeq {
		c.lastReadSeq = c.inMaxCtnSeq
		availabled := c.inQ.get(c.inMaxCtnSeq)
		availabled, _ = c.inQ.deleteBefore(availabled)
		for i := availabled; i != nil; i = i.next {
			c.inQReady = append(c.inQReady, i.payload...)
			// data was copied, then could recycle memory
			bpool.Put(i.buffer)
			i.payload = nil
			i.buffer = nil
		}
		return true
	}
	return false
}

// should not call this function concurrently.
func (c *Conn) Read(buf []byte) (nr int, err error) {
	for {
		if len(c.inQReady) > 0 {
			n := copy(buf, c.inQReady)
			c.inQReady = c.inQReady[n:]
			return n, nil
		}
		if !c.readInQ() {
			if c.rtmo > 0 {
				var tmo int64
				tmo, c.rtmo = c.rtmo, 0
				select {
				case _, y := <-c.evRead:
					if !y && len(c.inQReady) == 0 {
						return 0, io.EOF
					}
				case <-NewTimerChan(tmo):
					return 0, ErrIOTimeout
				}
			} else {
				// only when evRead is closed and inQReady is empty
				// then could reply eof
				if _, y := <-c.evRead; !y && len(c.inQReady) == 0 {
					return 0, io.EOF
				}
			}
		}
	}
}

// should not call this function concurrently.
func (c *Conn) Write(data []byte) (nr int, err error) {
	for len(data) > 0 && err == nil {
		//buf := make([]byte, MSS+AH_SIZE)
		buf := bpool.Get(MSS + AH_SIZE)
		body := buf[TH_SIZE+CH_SIZE:]
		n := copy(body, data)
		nr += n
		data = data[n:]
		pk := &packet{flag: F_DATA, payload: body[:n], buffer: buf[:AH_SIZE+n]}
		err = c.inputAndSend(pk)
	}
	return
}

func (c *Conn) LocalAddr() net.Addr {
	return c.sock.LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.dest
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	if d := t.UnixNano()/Millisecond - Now(); d > 0 {
		c.rtmo = d
	}
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	if d := t.UnixNano()/Millisecond - Now(); d > 0 {
		c.wtmo = d
	}
	return nil
}
