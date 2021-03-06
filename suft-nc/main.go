package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/spance/suft/protocol"
)

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Flags() | log.Lmicroseconds)
}

var timeWaiting int64
var waiting = make(chan *eofStatus, 2)

func main() {
	var raddr string
	var p suft.Params
	flag.StringVar(&p.LocalAddr, "l", "", "local")
	flag.StringVar(&raddr, "r", "", "remote")
	flag.BoolVar(&p.IsServ, "s", false, "is server")
	flag.BoolVar(&p.FastRetransmit, "fr", true, "FastRetransmit")
	flag.BoolVar(&p.FlatTraffic, "ft", true, "FlatTraffic")
	flag.Int64Var(&p.Bandwidth, "b", 2, "bandwidth in mbps")
	flag.IntVar(&p.Debug, "debug", 0, "debug")
	flag.BoolVar(&p.EnablePprof, "pprof", false, "pprof")
	flag.BoolVar(&p.Stacktrace, "stacktrace", false, "stacktrace")
	flag.Int64Var(&timeWaiting, "w", 0, "Timeout waiting for half-closed connection")
	flag.Parse()

	if !p.IsServ && raddr == "" {
		log.Fatalln("missing -r")
	}

	e, err := suft.NewEndpoint(&p)
	checkErr(err)
	defer e.Close()

	log.Println("start", e.Addr())
	var conn *suft.Conn
	if !p.IsServ { // client
		conn, err = e.Dial(raddr)
		checkErr(err)
		log.Println("connected to", conn.RemoteAddr())
		go writeOut(conn)
		go readIn(conn)
	} else {
		conn = e.Listen()
		log.Println("connected from", conn.RemoteAddr())
		go writeOut(conn)
		go readIn(conn)
	}

	var eof1, eof2 *eofStatus
	// there is one direction channel has been released
	eof1 = <-waiting
	log.Println(eof1.status)

	if timeWaiting > 0 {
	for1:
		for i, v := range [2]int64{1, timeWaiting} {
			select {
			case eof2 = <-waiting:
				// stdin reader released
				break for1
			case <-time.After(time.Duration(v * 1e9)):
				if i == 0 { // first second for preparing countdown
					log.Printf("the countdown to %c has started", "RW"[(eof1.channel+1)%2])
				} else { // kill
					conn.Close()
				}
			}
		}
	} else {
	for2:
		for { // special case for close stdin
			select {
			case eof2 = <-waiting:
				// stdin reader released
				break for2
			case <-time.After(1e9):
				// conn is closed but reading stdin was still blocking
				if conn.IsClosed() {
					break for2
				}
			}
		}
	}

	if eof2 != nil {
		log.Println(eof2.status)
	}
	conn.PrintState()
}

func readIn(c *suft.Conn) {
	var (
		n          int64
		err1, err2 error
	)
	wa := suft.StartWatch("R")
	n, err1 = io.Copy(c, os.Stdin)
	wa.Stop(int(n))

	if timeWaiting <= 0 {
		err2 = c.Close()
	}
	waiting <- &eofStatus{
		channel: 0,
		status:  fmt.Sprint("readIn done", err1, err2),
	}
}

func writeOut(c *suft.Conn) {
	var (
		n          int64
		err1, err2 error
	)
	wa := suft.StartWatch("W")
	n, err1 = io.Copy(os.Stdout, c)
	os.Stdout.Sync()
	wa.Stop(int(n))

	if timeWaiting <= 0 {
		err2 = c.Close()
	}
	waiting <- &eofStatus{
		channel: 1,
		status:  fmt.Sprint("writeOut done", err1, err2),
	}
}

func checkErr(e error) {
	if e != nil {
		log.Fatalln(e)
	}
}

type eofStatus struct {
	channel int
	status  string
}
