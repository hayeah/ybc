package memcache

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/valyala/ybc/bindings/go/ybc"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

func readCrLf(r *bufio.Reader) bool {
	if !readByte(r, '\r') {
		return false
	}
	return readByte(r, '\n')
}

func parseExptime(s []byte) (exptime time.Duration, ok bool) {
	t, err := strconv.Atoi(string(s))
	if err != nil {
		log.Printf("Cannot convert exptime=[%s] to integer: [%s]", s, err)
		ok = false
		return
	}
	if t == 0 {
		exptime = ybc.MaxTtl
	} else if t > 30*24*3600 {
		exptime = time.Unix(int64(t), 0).Sub(time.Now())
	} else {
		exptime = time.Second * time.Duration(t)
	}
	ok = true
	return
}

func clientError(w *bufio.Writer, s string) {
	fmt.Fprintf(w, "CLIENT_ERROR %s\r\n", s)
}

func serverError(w *bufio.Writer, s string) {
	fmt.Fprintf(w, "SERVER_ERROR %s\r\n", s)
}

func protocolError(w *bufio.Writer) {
	w.WriteString("ERROR\r\n")
}

func writeGetResponse(w *bufio.Writer, key []byte, item *ybc.Item) bool {
	_, err := w.Write([]byte("VALUE "))
	if err != nil {
		log.Printf("Error when writing VALUE response: [%s]", err)
		return false
	}
	_, err = w.Write(key)
	if err != nil {
		log.Printf("Error when writing key=[%s] to 'get' response: [%s]", key, err)
		return false
	}
	_, err = w.Write([]byte(" 0 "))
	if err != nil {
		log.Printf("Error when writing ' 0 ' to 'get' response: [%s]", err)
		return false
	}
	size := item.Size()
	_, err = w.Write([]byte(strconv.Itoa(size)))
	if err != nil {
		log.Printf("Error when writing size=[%d] to 'get' response: [%s]", size, err)
		return false
	}
	_, err = w.Write([]byte(" 0\r\n"))
	if err != nil {
		log.Printf("Error when writing 0\\r\\n to 'get' response: [%s]", err)
		return false
	}
	n, err := item.WriteTo(w)
	if err != nil {
		log.Printf("Error when writing payload: [%s]", err)
		return false
	}
	if n != int64(size) {
		log.Printf("Invalid length of payload=[%d]. Expected [%d]", n, size)
		return false
	}
	_, err = w.Write([]byte("\r\n"))
	if err != nil {
		log.Printf("Error when writing \\r\\n to response: [%s]", err)
		return false
	}
	return true
}

func getItemAndWriteResponse(w *bufio.Writer, cache ybc.Cacher, key []byte) bool {
	item, err := cache.GetItem(key)
	if err != nil {
		if err == ybc.ErrNotFound {
			return true
		}
		log.Fatalf("Unexpected error returned by cache.GetItem(): [%s]", err)
	}
	defer item.Close()

	return writeGetResponse(w, key, item)
}

func processGetCmd(c *bufio.ReadWriter, cache ybc.Cacher, line []byte) bool {
	last := -1
	lineSize := len(line)
	for last < lineSize {
		first := last + 1
		last = bytes.IndexByte(line[first:], ' ')
		if last == -1 {
			last = lineSize
		} else {
			last += first
		}
		if first == last {
			continue
		}
		key := line[first:last]
		if !getItemAndWriteResponse(c.Writer, cache, key) {
			return false
		}
	}

	_, err := c.Write([]byte("END\r\n"))
	if err != nil {
		log.Printf("Error when writing END to response: [%s]", err)
		return false
	}
	return true
}

type setCmd struct {
	key     []byte
	exptime []byte
	size    []byte
	noreply []byte
}

func parseSetCmd(line []byte, cmd *setCmd) bool {
	n := -1

	cmd.key, n = nextToken(line, n, "key")
	if cmd.key == nil {
		return false
	}
	flagsUnused, n := nextToken(line, n, "flags")
	if flagsUnused == nil {
		return false
	}
	cmd.exptime, n = nextToken(line, n, "exptime")
	if cmd.exptime == nil {
		return false
	}
	cmd.size, n = nextToken(line, n, "size")
	if cmd.size == nil {
		return false
	}

	if n == len(line) {
		return true
	}

	cmd.noreply, n = nextToken(line, n, "noreply")
	if cmd.noreply == nil {
		return false
	}
	return n == len(line)
}

func processSetCmd(c *bufio.ReadWriter, cache ybc.Cacher, line []byte, cmd *setCmd) bool {
	cmd.noreply = nil
	if !parseSetCmd(line, cmd) {
		clientError(c.Writer, "unrecognized 'set' command")
		return false
	}

	key := cmd.key
	exptime, ok := parseExptime(cmd.exptime)
	if !ok {
		clientError(c.Writer, "invalid exptime")
		return false
	}
	size, ok := parseSize(cmd.size)
	if !ok {
		clientError(c.Writer, "invalid size")
		return false
	}
	noreply := false
	if cmd.noreply != nil {
		if !bytes.Equal(cmd.noreply, []byte("noreply")) {
			clientError(c.Writer, "unrecognized noreply")
			return false
		}
		noreply = true
	}
	txn, err := cache.NewSetTxn(key, size, exptime)
	if err != nil {
		log.Printf("Cannot start 'set' transaction for key=[%s], size=[%d], exptime=[%d]: [%s]", key, size, exptime, err)
		serverError(c.Writer, "cannot start 'set' transaction")
		return false
	}
	defer txn.Commit()
	n, err := txn.ReadFrom(c.Reader)
	if err != nil {
		log.Printf("Error when reading payload for key=[%s], size=[%d]: [%s]", key, size, err)
		clientError(c.Writer, "cannot read payload")
		return false
	}
	if n != int64(size) {
		log.Printf("Unexpected payload size=[%d]. Expected [%d]", n, size)
		clientError(c.Writer, "unexpected payload size")
		return false
	}
	if !readCrLf(c.Reader) {
		clientError(c.Writer, "cannot read crlf after payload")
		return false
	}
	if !noreply {
		_, err = c.Write([]byte("STORED\r\n"))
		if err != nil {
			log.Printf("Error when writing response: [%s]", err)
			return false
		}
	}
	return true
}

func processRequest(c *bufio.ReadWriter, cache ybc.Cacher, lineBuf *[]byte, cmd *setCmd) bool {
	if !readLine(c.Reader, lineBuf) {
		protocolError(c.Writer)
		return false
	}
	line := *lineBuf
	if len(line) == 0 {
		return false
	}
	if bytes.HasPrefix(line, []byte("get ")) {
		return processGetCmd(c, cache, line[4:])
	}
	if bytes.HasPrefix(line, []byte("gets ")) {
		return processGetCmd(c, cache, line[5:])
	}
	if bytes.HasPrefix(line, []byte("set ")) {
		return processSetCmd(c, cache, line[4:], cmd)
	}
	log.Printf("Unrecognized command=[%s]", line)
	protocolError(c.Writer)
	return false
}

func handleConn(conn net.Conn, cache ybc.Cacher, readBufferSize, writeBufferSize int, done *sync.WaitGroup) {
	defer conn.Close()
	defer done.Done()
	r := bufio.NewReaderSize(conn, readBufferSize)
	w := bufio.NewWriterSize(conn, writeBufferSize)
	c := bufio.NewReadWriter(r, w)
	defer w.Flush()

	lineBuf := make([]byte, 0, 1024)
	cmd := setCmd{}
	for {
		if !processRequest(c, cache, &lineBuf, &cmd) {
			break
		}
		if r.Buffered() == 0 {
			w.Flush()
		}
	}
}

type Server struct {
	Cache           ybc.Cacher
	ListenAddr      string
	ReadBufferSize  int
	WriteBufferSize int

	listenSocket net.Listener
	done         *sync.WaitGroup
	err          error
}

func (s *Server) init() {
	if s.ReadBufferSize == 0 {
		s.ReadBufferSize = defaultReadBufferSize
	}
	if s.WriteBufferSize == 0 {
		s.WriteBufferSize = defaultWriteBufferSize
	}

	var err error
	s.listenSocket, err = net.Listen("tcp", s.ListenAddr)
	if err != nil {
		log.Fatal("Cannot listen for ListenAddr=[%s]: [%s]", s.ListenAddr, err)
	}
	s.done = &sync.WaitGroup{}
	s.done.Add(1)
}

func (s *Server) run() {
	defer s.done.Done()

	connsDone := &sync.WaitGroup{}
	defer connsDone.Wait()
	for {
		conn, err := s.listenSocket.Accept()
		if err != nil {
			s.err = err
			break
		}
		connsDone.Add(1)
		go handleConn(conn, s.Cache, s.ReadBufferSize, s.WriteBufferSize, connsDone)
	}
}

func (s *Server) Start() {
	if s.listenSocket != nil || s.done != nil {
		panic("Did you forgot calling Server.Stop() before calling Server.Start()?")
	}
	s.init()
	go s.run()
}

func (s *Server) Wait() error {
	s.done.Wait()
	return s.err
}

func (s *Server) Serve() error {
	s.Start()
	return s.Wait()
}

func (s *Server) Stop() {
	s.listenSocket.Close()
	s.Wait()
	s.listenSocket = nil
	s.done = nil
}
