package transport

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/types"
	"golang.org/x/crypto/ssh"
)

type SSHSession struct {
	In      io.Reader
	Out     io.WriteCloser
	Session *ssh.Session
}

type SSHOption func(*SSHTransport) error

// The reply the execute command and the prompt.
type SSHReply struct{ result, prompt, command string }

// SSHTransport setting needs to be set before calling Connect()
// SSHTransport implements the Transport interface
type SSHTransport struct {
	// Channel used to read. Can use Expect to Write & read wit timeout
	in chan SSHReply
	// SSH Session
	ses *SSHSession
	// Contains the first read after connecting
	LoginMessage *SSHReply
	// SSH parameters used in connect
	// defualt: 22
	Port int

	// Keep the target for logging
	Target string

	// SSH Options
	// required!
	SSHConfig *ssh.ClientConfig

	// Character to split the incoming stream (#/$/>)
	// default: #
	PromptChar string

	// Kind specific transactions & prompt checking function
	K SSHKind
}

func NewSSHTransport(node *types.Node, options ...SSHOption) (*SSHTransport, error) {
	switch node.Kind {
	case "vr-sros", "srl":
		c := &SSHTransport{}
		c.SSHConfig = &ssh.ClientConfig{}

		// apply options
		for _, opt := range options {
			opt(c)
		}

		switch node.Kind {
		case "vr-sros":
			c.K = &VrSrosSSHKind{}
		case "srl":
			c.K = &SrlSSHKind{}
		}
		return c, nil
	}
	return nil, fmt.Errorf("no tranport implemented for kind: %s", node.Kind)
}

// Creates the channel reading the SSH connection
//
// The first prompt is saved in LoginMessages
//
// - The channel read the SSH session, splits on PromptChar
// - Uses SSHKind's PromptParse to split the received data in *result* and *prompt* parts
//   (if no valid prompt was found, prompt will simply be empty and result contain all the data)
// - Emit data
func (t *SSHTransport) InChannel() {
	// Ensure we have a working channel
	t.in = make(chan SSHReply)

	// setup a buffered string channel
	go func() {
		buf := make([]byte, 1024)
		tmpS := ""
		n, err := t.ses.In.Read(buf) //this reads the ssh terminal
		if err == nil {
			tmpS = string(buf[:n])
		}
		for err == nil {

			if strings.Contains(tmpS, "#") {
				parts := strings.Split(tmpS, "#")
				li := len(parts) - 1
				for i := 0; i < li; i++ {
					r := t.K.PromptParse(t, &parts[i])
					if r == nil {
						r = &SSHReply{
							result: parts[i],
						}
					}
					t.in <- *r
				}
				tmpS = parts[li]
			}
			n, err = t.ses.In.Read(buf)
			tmpS += string(buf[:n])
		}
		log.Debugf("In Channel closing: %v", err)
		t.in <- SSHReply{
			result: tmpS,
			prompt: "",
		}
	}()

	// Save first prompt
	t.LoginMessage = t.Run("", 15)
	if DebugCount > 1 {
		t.LoginMessage.Info(t.Target)
	}
}

// Run a single command and wait for the reply
func (t *SSHTransport) Run(command string, timeout int) *SSHReply {
	if command != "" {
		t.ses.Writeln(command)
		log.Debugf("--> %s\n", command)
	}

	sHistory := ""

	for {
		// Read from the channel with a timeout
		var rr string

		select {
		case <-time.After(time.Duration(timeout) * time.Second):
			log.Warnf("timeout waiting for prompt: %s", command)
			return &SSHReply{
				result:  sHistory,
				command: command,
			}
		case ret := <-t.in:
			if DebugCount > 1 {
				ret.Debug(t.Target, command+"<--InChannel--")
			}

			if ret.result == "" && ret.prompt == "" {
				log.Fatalf("received zero?")
				continue
			}

			if ret.prompt == "" && ret.result != "" {
				// we should continue reading...
				sHistory += ret.result
				if DebugCount > 1 {
					log.Debugf("+")
				}
				timeout = 2 // reduce timeout, node is already sending data
				continue
			}

			if sHistory == "" {
				rr = ret.result
			} else {
				rr = sHistory + "#" + ret.result
				sHistory = ""
			}
			rr = strings.Trim(rr, " \n\r\t")

			if strings.HasPrefix(rr, command) {
				rr = strings.Trim(rr[len(command):], " \n\r\t")
			} else if !strings.Contains(rr, command) {
				log.Debugf("read more %s:%s", command, rr)
				sHistory = rr
				continue
			}
			res := &SSHReply{
				result:  rr,
				prompt:  ret.prompt,
				command: command,
			}
			res.Debug(t.Target, command+"<--RUN--")
			return res
		}
	}
}

// Write a config snippet (a set of commands)
// Session NEEDS to be configurable for other kinds
// Part of the Transport interface
func (t *SSHTransport) Write(data, info *string) error {
	if *data == "" {
		return nil
	}

	transaction := !strings.HasPrefix(*info, "show-")

	err := t.K.ConfigStart(t, transaction)
	if err != nil {
		return err
	}

	c := 0

	for _, l := range strings.Split(*data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		c += 1
		t.Run(l, 5).Info(t.Target)
	}

	if transaction {
		commit, err := t.K.ConfigCommit(t)
		msg := fmt.Sprintf("%s COMMIT - %d lines", *info, c)
		if commit.result != "" {
			msg += commit.LogString(t.Target, true, false)
		}
		if err != nil {
			log.Error(msg)
			return err
		}
		log.Info(msg)
	}

	return nil
}

// Connect to a host
// Part of the Transport interface
func (t *SSHTransport) Connect(host string, options ...func(*Transport)) error {
	// Assign Default Values
	if t.PromptChar == "" {
		t.PromptChar = "#"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.SSHConfig == nil {
		return fmt.Errorf("require auth credentials in SSHConfig")
	}

	// Start some client config
	host = fmt.Sprintf("%s:%d", host, t.Port)
	//sshConfig := &ssh.ClientConfig{}
	//SSHConfigWithUserNamePassword(sshConfig, "admin", "admin")

	t.Target = strings.Split(strings.Split(host, ":")[0], "-")[2]

	ses_, err := NewSSHSession(host, t.SSHConfig)
	if err != nil || ses_ == nil {
		return fmt.Errorf("cannot connect to %s: %s", host, err)
	}
	t.ses = ses_

	log.Infof("Connected to %s\n", host)
	t.InChannel()
	//Read to first prompt
	return nil
}

// Close the Session and channels
// Part of the Transport interface
func (t *SSHTransport) Close() {
	if t.in != nil {
		close(t.in)
		t.in = nil
	}
	t.ses.Close()
}

// Add a basic username & password to a config.
// Will initilize the config if required
func WithUserNamePassword(username, password string) SSHOption {
	return func(tx *SSHTransport) error {
		if tx.SSHConfig == nil {
			tx.SSHConfig = &ssh.ClientConfig{}
		}
		tx.SSHConfig.User = username
		if tx.SSHConfig.Auth == nil {
			tx.SSHConfig.Auth = []ssh.AuthMethod{}
		}
		tx.SSHConfig.Auth = append(tx.SSHConfig.Auth, ssh.Password(password))
		tx.SSHConfig.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		return nil
	}
}

// Create a new SSH session (Dial, open in/out pipes and start the shell)
// pass the authntication details in sshConfig
func NewSSHSession(host string, sshConfig *ssh.ClientConfig) (*SSHSession, error) {
	if !strings.Contains(host, ":") {
		return nil, fmt.Errorf("include the port in the host: %s", host)
	}

	connection, err := ssh.Dial("tcp", host, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %s", err)
	}
	session, err := connection.NewSession()
	if err != nil {
		return nil, err
	}
	sshIn, err := session.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("session stdout: %s", err)
	}
	sshOut, err := session.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("session stdin: %s", err)
	}
	// sshIn2, err := session.StderrPipe()
	// if err != nil {
	// 	return nil, fmt.Errorf("session stderr: %s", err)
	// }
	// Request PTY (required for srl)
	modes := ssh.TerminalModes{
		ssh.ECHO: 1, // disable echo
	}
	err = session.RequestPty("dumb", 24, 100, modes)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("pty request failed: %s", err)
	}

	if err := session.Shell(); err != nil {
		session.Close()
		return nil, fmt.Errorf("session shell: %s", err)
	}

	return &SSHSession{
		Session: session,
		In:      sshIn,
		Out:     sshOut,
	}, nil
}

func (ses *SSHSession) Writeln(command string) (int, error) {
	return ses.Out.Write([]byte(command + "\r"))
}

func (ses *SSHSession) Close() {
	log.Debugf("Closing session")
	ses.Session.Close()
}

// The LogString will include the entire SSHReply
//   Each field will be prefixed by a character.
//   # - command sent
//   | - result recieved
//   ? - prompt part of the result
func (r *SSHReply) LogString(node string, linefeed, debug bool) string {
	ind := 12 + len(node)
	prefix := "\n" + strings.Repeat(" ", ind)
	s := ""
	if linefeed {
		s = "\n" + strings.Repeat(" ", 11)
	}
	s += node + " # " + r.command
	s += prefix + "| "
	s += strings.Join(strings.Split(r.result, "\n"), prefix+"| ")
	if debug { // Add the prompt & more
		s = "" + strings.Repeat(" ", ind) + s
		s += prefix + "? "
		s += strings.Join(strings.Split(r.prompt, "\n"), prefix+"? ")
		if DebugCount > 3 { // add bytestring
			s += fmt.Sprintf("%s| %v%s ? %v", prefix, []byte(r.result), prefix, []byte(r.prompt))
		}

	}
	return s
}

func (r *SSHReply) Info(node string) *SSHReply {
	if r.result == "" {
		return r
	}
	log.Info(r.LogString(node, false, false))
	return r
}

func (r *SSHReply) Debug(node, message string, t ...interface{}) {
	msg := message
	if len(t) > 0 {
		msg = t[0].(string)
	}
	_, fn, line, _ := runtime.Caller(1)
	msg += fmt.Sprintf("(%s line %d)", fn, line)
	msg += r.LogString(node, true, true)
	log.Debugf(msg)
}
