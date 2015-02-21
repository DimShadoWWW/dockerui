package main // import "github.com/DimShadoWWW/dockerui"

import (
	"encoding/json"
	"flag"
	"github.com/gorilla/mux"
	"github.com/hashicorp/memberlist"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
)

var (
	endpoint  = flag.String("e", "/var/run/docker.sock", "Dockerd endpoint")
	addr      = flag.String("p", ":9000", "Address and port to serve dockerui")
	nodes     = flag.String("n", "127.0.0.1", "Address of docker's cluster nodes with tcp socket : 127.0.0.1,192.168.0.1...  (csv)")
	assets    = flag.String("a", ".", "Path to the assets")
	endpoints EndpointsSet
)

// +gen set
type Endpoints string

type Config struct {
	endpoints []string
}

// Wraps server muxer, dynamic map of handlers, and listen port.
type Server struct {
	Dispatcher *mux.Router
	Urls       map[string]func(w http.ResponseWriter, r *http.Request)
	Address    string
}

type UnixHandler struct {
	path string
}

func (h *UnixHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := net.Dial("unix", h.path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println(err)
		return
	}
	c := httputil.NewClientConn(conn, nil)
	defer c.Close()

	res, err := c.Do(r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println(err)
		return
	}
	defer res.Body.Close()

	copyHeader(w.Header(), res.Header)
	if _, err := io.Copy(w, res.Body); err != nil {
		log.Println(err)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func createTcpHandler(e string) http.Handler {
	u, err := url.Parse(e)
	if err != nil {
		log.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

func createUnixHandler(e string) http.Handler {
	return &UnixHandler{e}
}

func createHandler(dir string, e string) http.Handler {
	var (
		h http.Handler
	)

	if strings.Contains(e, "http") {
		h = createTcpHandler(e)
	} else {
		if _, err := os.Stat(e); err != nil {
			if os.IsNotExist(err) {
				log.Fatalf("unix socket %s does not exist", e)
			}
			log.Fatal(err)
		}
		h = createUnixHandler(e)
	}

	return h
}

func (s *Server) Start() {

	http.ListenAndServe(s.Address, s.Dispatcher)
}

// Initialize Dispatcher's routes.
func (s *Server) InitDispatch(dir string, e string) {
	var (
		fileHandler = http.FileServer(http.Dir(dir))
		// h           http.Handler
	)

	d := s.Dispatcher

	// if _, err := os.Stat(e); err != nil {
	// 	if os.IsNotExist(err) {
	// 		log.Fatalf("unix socket %s does not exist", e)
	// 	}
	// 	log.Fatal(err)
	// }
	// h = createUnixHandler(e)

	// d.HandleFunc("/dockerapi/{name}", func(w http.ResponseWriter, r *http.Request) {
	// 	//Lookup handler in map and call it, proxying this writer and request
	// 	vars := mux.Vars(r)
	// 	name := vars["name"]

	// 	s.ProxyCall(w, r, name)
	// })
	d.Handle("/", fileHandler)
	d.HandleFunc("/endpoints", func(w http.ResponseWriter, r *http.Request) {
		var ep []string
		for e := range endpoints {
			if e != "127.0.0.1" {
				ep = append(ep, e)
			}
		}
		result, err := json.Marshal(ep)
		if err != nil {
			return
		}

		//rr, err := json.NewEncoder(c.ResponseWriter).Encode(m)
		w.Header().Set("Content-Length", strconv.Itoa(len(result)))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, string(result))
	})

	d.Handle("/localdockerapi", createHandler(*assets, *endpoint))

	d.HandleFunc("/dockerapi/{name}", func(w http.ResponseWriter, r *http.Request) {
		//Lookup handler in map and call it, proxying this writer and request
		vars := mux.Vars(r)
		name := vars["name"]

		s.ProxyCall(w, r, name)
	})

	go s.NodesUpdater()
}

func (s *Server) Destroy(fName string) {
	s.Urls[fName] = nil //remove handler
}

func (s *Server) ProxyCall(w http.ResponseWriter, r *http.Request, fName string) {
	if s.Urls[fName] != nil {
		s.Urls[fName](w, r) //proxy the call
	}
}

func (s *Server) NodesUpdater() {
	eventCh := make(chan memberlist.NodeEvent, 16)

	config := memberlist.DefaultLANConfig()
	config.Events = &memberlist.ChannelEventDelegate{eventCh}
	list, err := memberlist.Create(config)
	if err != nil {
		panic("Failed to create memberlist: " + err.Error())
	}

	// Join an existing cluster by specifying at least one known member.
	endpoints = NewEndpointsSet(strings.Split(*nodes, ","))
	_, err = list.Join(endpoints.ToSlice())
	if err != nil {
		panic("Failed to join cluster: " + err.Error())
	}

	// Ask for members of the cluster
	for {
		select {
		case e := <-eventCh:
			if e.Event == memberlist.NodeJoin {
				log.Printf("[DEBUG] Node join: %v", *e.Node)
				laddr := e.Node.Addr.String()
				endpoints.Add(laddr)
				s.Urls[e.Node.Name] = createTcpHandler("http://" + laddr + ":" + strings.Split(*addr, ":")[1] + "/dockerapi/local").ServeHTTP // Add the handler to our map
			} else {
				log.Printf("[DEBUG] Node leave: %v", *e.Node)
				laddr := e.Node.Addr.String()
				endpoints.Remove(laddr)
				s.Urls[e.Node.Name] = nil
			}
			// save member list to file
		}
	}

}

func main() {
	flag.Parse()

	// handler := createHandler(*assets, *endpoint)
	// if err := http.ListenAndServe(*addr, handler); err != nil {
	// 	log.Fatal(err)
	// }

	//Initialize Server
	server := &Server{Address: ":3000", Dispatcher: mux.NewRouter(), Urls: make(map[string]func(w http.ResponseWriter, r *http.Request))}

	flag.Parse()

	server.Address = *addr

	log.Printf("Starting server on address: %s \n", server.Address)

	server.InitDispatch(*assets, *endpoint)
	log.Printf("Initializing request routes...\n")

	server.Start() //Launch server; blocks goroutine.
}
