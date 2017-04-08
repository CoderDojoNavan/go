package api

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"

	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pb "github.com/coderdojonavan/go/chatapp/proto"
)

type connection struct {
	conn    *grpc.ClientConn
	client  pb.ChatAppServiceClient
	token   string
	name    string
	ctx     context.Context
	handler func(http.ResponseWriter, *http.Request)
}

var c *connection

// Register the bot with the server
func Register(addr string, name string) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	var err error
	c = &connection{
		ctx: context.Background(),
	}
	c.conn, err = grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		fmt.Printf("Unable to contact server: %v\n", err)
		os.Exit(1)
	}
	c.client = pb.NewChatAppServiceClient(c.conn)

	c.name = name

	// Actually register
	r, err := c.client.Register(c.ctx, &pb.RegisterRequest{
		Name: name,
	})
	if err != nil {
		fmt.Printf("Cannot register with server: %v", err)
		c.conn.Close()
		os.Exit(1)
	}

	if r.GetToken() == "" {
		fmt.Println("Did not get token, exiting.")
		c.conn.Close()
		os.Exit(1)
	}

	c.token = r.GetToken()
	go Connect()
}

// Disconnect from the server
func Disconnect(msg string) {
	c.client.Disconnect(c.ctx, &pb.Message{
		Token: c.token,
		Data:  []byte(msg),
	})
}

// HandleFunc registers a function with a similar footprint as
// net/http.HandleFunc
func HandleFunc(handler func(http.ResponseWriter, *http.Request)) {
	c.handler = handler
}

type writer struct {
}

func (w writer) Header() http.Header {
	return http.Header{}
}

func (w writer) Write(d []byte) (int, error) {
	go broadcast(d)
	return len(d), nil
}

func (w writer) WriteHeader(int) {
}

func broadcast(msg []byte) {
	_, err := c.client.Broadcast(c.ctx, &pb.Message{
		Data:  msg,
		Token: c.token,
	})
	if err != nil {
		log.Printf("Unable to send message: %v", err)
	}
}

// Connect to the server
func Connect() {
	stream, err := c.client.Listen(c.ctx, &pb.Message{
		Data:  []byte(fmt.Sprintf("%s has joined the chat", c.name)),
		Token: c.token,
	})
	if err != nil {
		log.Printf("Can't establish stream: %v", err)
	}

	for {
		data, err := stream.Recv()
		if err != nil {
			log.Printf("Can't get data: %v", err)
		}
		log.Println("got", data)

		req := &http.Request{}
		u, _ := url.Parse("http://chatbot/?msg=" + string(data.Data))
		req.URL = u

		res := writer{}
		c.handler(res, req)
	}

}
