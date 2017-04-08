// Package main contains the main entry point for the application
package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pb "github.com/coderdojonavan/go/chatapp/proto"
)

var (
	bindFlag = flag.String("bind", "0.0.0.0", "Address to bind the server to")
	portFlag = flag.Int("port", 1234, "port to run the server on")
)

type message struct {
	name string
	data []byte
}

type server struct {
	bots        map[string]string
	users       map[*Client]bool
	clientChans map[string](chan message)
	lis         net.Listener
	srv         *grpc.Server

	// Inbound messages from the clients.
	outbound chan []byte
	// Register requests from the clients.
	register chan *Client
	// Unregister requests from clients.
	unregister chan *Client
}

func token() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (s *server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	found := false
	token := token()
	for t, n := range s.bots {
		if n == req.GetName() {
			token = t
			found = true
			go s.broadcast(n, "has rejoined the chat")
			break
		}
	}

	if !found {
		s.bots[token] = req.GetName()
	}

	return &pb.RegisterResponse{Token: token}, nil
}

func (s *server) Broadcast(ctx context.Context, req *pb.Message) (*pb.Empty, error) {
	n, ok := s.bots[req.GetToken()]
	if !ok {
		return &pb.Empty{}, fmt.Errorf("No bot found with token %s", req.GetToken())
	}
	go s.broadcast(n, string(req.GetData()))
	return &pb.Empty{}, nil
}

func (s *server) Listen(req *pb.Message, stream pb.ChatAppService_ListenServer) error {
	out := make(chan message)
	s.clientChans[req.GetToken()] = out
	for {
		msg := <-out
		err := stream.Send(&pb.Message{
			Name: msg.name,
			Data: msg.data,
		})
		if err != nil {
			log.Println(err)
		}
	}
}

func (s *server) Disconnect(ctx context.Context, req *pb.Message) (*pb.Empty, error) {
	n, ok := s.bots[req.GetToken()]
	if !ok {
		return &pb.Empty{}, fmt.Errorf("No such token")
	}
	s.broadcast(n, fmt.Sprintf("disconnected - %s", string(req.Data)))
	delete(s.bots, req.GetToken())
	delete(s.clientChans, req.GetToken())

	return &pb.Empty{}, nil
}

func (s *server) broadcast(name, msg string) {
	out := fmt.Sprintf("%s: %s", name, string(msg))
	log.Println("Chat |", out)
	for _, c := range s.clientChans {
		c <- message{name, []byte(msg)}
	}
	for u, ok := range s.users {
		if ok {
			u.send <- []byte(out)
		}
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	log.Println(r.URL)
	if r.URL.Path != "/" {
		http.Error(w, "Not found", 404)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	http.ServeFile(w, r, "home.html")
}

////////////////////////////////////////////////////////////////////////////////

func (s *server) run() {
	for {
		select {
		case user := <-s.register:
			s.users[user] = true
		case user := <-s.unregister:
			if _, ok := s.users[user]; ok {
				delete(s.users, user)
				close(user.send)
			}
		case message := <-s.outbound:
			s.broadcast("User", string(message))
			// for user := range s.users {
			// 	select {
			// 	// case user.send <- message:
			// 	default:
			// 		close(user.send)
			// 		delete(s.users, user)
			// 	}
			// }
		}
	}
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	hub *server

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Printf("error: %v", err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		c.hub.outbound <- message
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued chat messages to the current websocket message.
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(hub *server, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	client := &Client{hub: hub, conn: conn, send: make(chan []byte, 256)}
	client.hub.register <- client
	go client.writePump()
	client.readPump()
}

////////////////////////////////////////////////////////////////////////////////
func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	var err error
	s := new(server)
	s.bots = make(map[string]string)
	s.clientChans = make(map[string](chan message))

	s.users = make(map[*Client]bool)
	s.outbound = make(chan []byte)
	s.register = make(chan *Client)
	s.unregister = make(chan *Client)

	s.lis, err = net.Listen("tcp", fmt.Sprintf("%s:%d", *bindFlag, *portFlag))
	if err != nil {
		log.Fatal(err)
	}

	s.srv = grpc.NewServer()
	defer s.srv.Stop()

	pb.RegisterChatAppServiceServer(s.srv, s)
	go s.srv.Serve(s.lis)

	go s.run()
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(s, w, r)
	})
	log.Println(http.ListenAndServe(":8080", handlers.LoggingHandler(os.Stdout, http.DefaultServeMux)))
}
