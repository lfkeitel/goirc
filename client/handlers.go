package client

// this file contains the basic set of event handlers
// to manage tracking an irc connection etc.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lfkeitel/goirc/logging"
)

// sets up the internal event handlers to do essential IRC protocol things
var intHandlers = map[string]HandlerFunc{
	REGISTER: (*Conn).h_REGISTER,
	"001":    (*Conn).h_001,
	"433":    (*Conn).h_433,
	CTCP:     (*Conn).h_CTCP,
	NICK:     (*Conn).h_NICK,
	PING:     (*Conn).h_PING,
}

func (conn *Conn) addIntHandlers() {
	for n, h := range intHandlers {
		// internal handlers are essential for the IRC client
		// to function, so we don't save their Removers here
		conn.handle(n, h)
	}
}

// Basic ping/pong handler
func (conn *Conn) h_PING(line *Line) {
	conn.Pong(line.Args[0])
}

// Handler for initial registration with server once tcp connection is made.
func (conn *Conn) h_REGISTER(line *Line) {
	// Temporary disable flood control for login and negotiation
	oldFlood := conn.cfg.Flood
	conn.cfg.Flood = true
	defer func() { conn.cfg.Flood = oldFlood }()

	if conn.cfg.Pass != "" {
		conn.Pass(conn.cfg.Pass)
	}

	if err := conn.negotiateCaps(); err != nil {
		logging.Error("%s", err)
		conn.Close()
		return
	}

	conn.Nick(conn.cfg.Me.Nick)
	conn.User(conn.cfg.Me.Ident, conn.cfg.Me.Name)
}

func (conn *Conn) negotiateCaps() error {
	saslResChan := make(chan *SASLResult)
	if conn.cfg.UseSASL {
		conn.cfg.RequestCaps = append(conn.cfg.RequestCaps, "sasl")
		conn.setupSASLCallbacks(saslResChan)
	}

	if len(conn.cfg.RequestCaps) == 0 {
		return nil
	}

	capChann := make(chan bool, len(conn.cfg.RequestCaps))
	conn.HandleFunc(CAP, func(conn *Conn, line *Line) {
		if len(line.Args) != 3 {
			return
		}
		command := line.Args[1]

		if command == "LS" {
			missingCaps := len(conn.cfg.RequestCaps)
			for _, capName := range strings.Split(line.Args[2], " ") {
				for _, reqCap := range conn.cfg.RequestCaps {
					if capName == reqCap {
						conn.Raw(fmt.Sprintf("CAP REQ :%s", capName))
						missingCaps--
					}
				}
			}

			for i := 0; i < missingCaps; i++ {
				capChann <- true
			}
		} else if command == "ACK" || command == "NAK" {
			for _, capName := range strings.Split(strings.TrimSpace(line.Args[2]), " ") {
				if capName == "" {
					continue
				}

				if command == "ACK" {
					conn.AcknowledgedCaps = append(conn.AcknowledgedCaps, capName)
				}
				capChann <- true
			}
		}
	})

	conn.Raw("CAP LS")

	if conn.cfg.UseSASL {
		select {
		case res := <-saslResChan:
			if res.Failed {
				close(saslResChan)
				return res.Err
			}
		case <-time.After(time.Second * 15):
			close(saslResChan)
			return errors.New("SASL setup timed out. This shouldn't happen.")
		}
	}

	// Wait for all capabilities to be ACKed or NAKed before ending negotiation
	for i := 0; i < len(conn.cfg.RequestCaps); i++ {
		<-capChann
	}
	conn.Raw("CAP END")
	return nil
}

// Handler to trigger a CONNECTED event on receipt of numeric 001
func (conn *Conn) h_001(line *Line) {
	// we're connected!
	conn.dispatch(&Line{Cmd: CONNECTED, Time: time.Now()})
	// and we're being given our hostname (from the server's perspective)
	t := line.Args[len(line.Args)-1]
	if idx := strings.LastIndex(t, " "); idx != -1 {
		t = t[idx+1:]
		if idx = strings.Index(t, "@"); idx != -1 {
			if conn.st != nil {
				me := conn.Me()
				conn.st.NickInfo(me.Nick, me.Ident, t[idx+1:], me.Name)
			} else {
				conn.cfg.Me.Host = t[idx+1:]
			}
		}
	}
}

// XXX: do we need 005 protocol support message parsing here?
// probably in the future, but I can't quite be arsed yet.
/*
	:irc.pl0rt.org 005 GoTest CMDS=KNOCK,MAP,DCCALLOW,USERIP UHNAMES NAMESX SAFELIST HCN MAXCHANNELS=20 CHANLIMIT=#:20 MAXLIST=b:60,e:60,I:60 NICKLEN=30 CHANNELLEN=32 TOPICLEN=307 KICKLEN=307 AWAYLEN=307 :are supported by this server
	:irc.pl0rt.org 005 GoTest MAXTARGETS=20 WALLCHOPS WATCH=128 WATCHOPTS=A SILENCE=15 MODES=12 CHANTYPES=# PREFIX=(qaohv)~&@%+ CHANMODES=beI,kfL,lj,psmntirRcOAQKVCuzNSMT NETWORK=bb101.net CASEMAPPING=ascii EXTBAN=~,cqnr ELIST=MNUCT :are supported by this server
	:irc.pl0rt.org 005 GoTest STATUSMSG=~&@%+ EXCEPTS INVEX :are supported by this server
*/

// Handler to deal with "433 :Nickname already in use"
func (conn *Conn) h_433(line *Line) {
	// Args[1] is the new nick we were attempting to acquire
	me := conn.Me()
	neu := conn.cfg.NewNick(line.Args[1])
	conn.Nick(neu)
	if !line.argslen(1) {
		return
	}
	// if this is happening before we're properly connected (i.e. the nick
	// we sent in the initial NICK command is in use) we will not receive
	// a NICK message to confirm our change of nick, so ReNick here...
	if line.Args[1] == me.Nick {
		if conn.st != nil {
			conn.cfg.Me = conn.st.ReNick(me.Nick, neu)
		} else {
			conn.cfg.Me.Nick = neu
		}
	}
}

// Handle VERSION requests and CTCP PING
func (conn *Conn) h_CTCP(line *Line) {
	if line.Args[0] == VERSION {
		conn.CtcpReply(line.Nick, VERSION, conn.cfg.Version)
	} else if line.Args[0] == PING && line.argslen(2) {
		conn.CtcpReply(line.Nick, PING, line.Args[2])
	}
}

// Handle updating our own NICK if we're not using the state tracker
func (conn *Conn) h_NICK(line *Line) {
	if conn.st == nil && line.Nick == conn.cfg.Me.Nick {
		conn.cfg.Me.Nick = line.Args[0]
	}
}
