// Copyright (c) 2015 Datacratic. All rights reserved.

package redis

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datacratic/goblueprint/blueprint"
)

// DefaultMaximumRedirections defines the default maximum number of times a request can be redirected to another node before failing.
var DefaultMaximumRedirections = 16

// DefaultMaximumSlotUpdates defines the number of MOVED it takes for the client to request a full resync of the cluster state.
var DefaultMaximumSlotUpdates = 4

// Client implements a client to the Redis database or cluster.
// This client always starts as a normal connection and migrates to handling cluster transparently when required.
// The first address is used to connect while the others can be used as alternatives in case of failure.
type Client struct {
	Address                   []string
	MaximumRedirections       int
	MaximumSlotUpdates        int
	MaximumConcurrentRequests int
	MaximumPendingRequests    int
	MaximumConnectionRetries  int
	RetryTimeout              time.Duration

	lua map[string]string

	state atomic.Value
	mu    sync.Mutex
	once  sync.Once
	nodes map[string]*Conn
}

type mapping struct {
	id     int64
	missed int
	shards bool
	closed bool
	nodes  map[string]*Conn
	slots  [16384]*Conn
}

func (client *Client) initialize() {
	// by default it will try to connect to the local Redis
	address := client.Address
	if len(address) == 0 {
		address = []string{"tcp://127.0.0.1:6379"}
	}

	client.nodes = make(map[string]*Conn)

	// prepare to (lazy) connect with all the nodes
	for i := range address {
		client.nodes[address[i]] = client.connect(address[i])
	}

	// create the initial state from the first address given in parameters
	primary := client.nodes[address[0]]
	state := &mapping{
		nodes: client.nodes,
	}

	for i, n := 0, len(state.slots); i < n; i++ {
		state.slots[i] = primary
	}

	client.state.Store(state)
	return
}

// Do executes the specified command (with optional arguments) to the Redis instance and waits to decode the reply.
func (client *Client) Do(name string, args ...interface{}) (result interface{}, err error) {
	request := NewRequest(name, args...)
	if err = client.Send(request); err == nil {
		result = request.commands[len(request.commands)-1].result
	}

	return
}

// Send sends the specified request to the Redis instance and waits for the reply.
func (client *Client) Send(request *Request) (err error) {
	value := client.state.Load()
	if value == nil {
		client.once.Do(client.initialize)
		value = client.state.Load()
	}

	state := value.(*mapping)
	if state.closed {
		log.Panicf("client closed")
	}

	// figure out where this request should be sent
	slot := 0
	if state.shards {
		slot = request.slot()
	}

	node := state.slots[slot]

	redirect := client.MaximumRedirections
	if 0 == redirect {
		redirect = DefaultMaximumRedirections
	}

	for i := 0; i < redirect; i++ {
		if node == nil {
			break
		}

		if err = node.Send(request); err == nil {
			break
		}

		// done?
		if !request.redirect {
			break
		}

		// migrate from a Redis client to a Redis cluster client
		if !state.shards {
			if state, err = client.migrate(); err != nil {
				return
			}

			slot = request.slot()
			node = state.slots[slot]
			continue
		}

		// already connected?
		if node = state.nodes[request.address]; node != nil {
			if request.moved {
				state, err = client.update(slot, node)
			}

			continue
		}

		state, node, err = client.redirect(request)
		if err != nil {
			node = client.random()
		}
	}

	return
}

// LuaScript loads a script into the script cache.
func (client *Client) LuaScript(code string) (id string, err error) {
	value := client.state.Load()
	if value == nil {
		client.once.Do(client.initialize)
		value = client.state.Load()
	}

	state := value.(*mapping)
	if state.closed {
		log.Panicf("client closed")
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	done := make(chan func() (interface{}, error))

	// load the script on all known connections
	for name := range client.nodes {
		name := name
		go func() {
			node := client.nodes[name]
			result, err := node.LuaScript(code)
			done <- func() (interface{}, error) {
				return result, err
			}
		}()
	}

	// wait for the result
	for i, n := 0, len(client.nodes); i < n; i++ {
		f := <-done
		var result interface{}
		result, err = f()
		if err != nil {
			return
		}

		key := result.(string)
		if id == "" || id == key {
			id = key
		} else {
			err = fmt.Errorf("script SHA1 doesn't match '%s' vs. '%s'", id, key)
		}
	}

	if id != "" {
		err = nil
	} else {
		err = fmt.Errorf("failed to get SHA1 from connections")
	}

	// remember this script for new connections
	if client.lua == nil {
		client.lua = make(map[string]string)
	}

	client.lua[id] = code
	return
}

// Close tears down the connection to the Redis database or cluster.
func (client *Client) Close() {
	if client == nil {
		return
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, item := range client.nodes {
		item.Close()
	}

	client.nodes = nil
	client.state.Store(&mapping{
		closed: true,
	})
}

func (client *Client) connect(address string) *Conn {
	lua := make(map[string]string)
	for key, code := range client.lua {
		lua[key] = code
	}

	return &Conn{
		MaximumConcurrentRequests: client.MaximumConcurrentRequests,
		MaximumPendingRequests:    client.MaximumPendingRequests,
		MaximumConnectionRetries:  client.MaximumConnectionRetries,
		RetryTimeout:              client.RetryTimeout,
		db: dialerFunc(func() (net.Conn, error) {
			u, err := url.Parse(address)
			if err != nil {
				return nil, err
			}

			return net.Dial(u.Scheme, u.Host+u.Path)
		}),
		lua: lua,
	}
}

func (client *Client) migrate() (state *mapping, err error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	state = client.state.Load().(*mapping)

	// already migrated?
	if state.shards {
		return
	}

	// update the mapping then
	state, err = client.reconfigure(state, state.slots[0])
	return
}

func (client *Client) update(slot int, node *Conn) (state *mapping, err error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	miss := client.MaximumSlotUpdates
	if 0 == miss {
		miss = DefaultMaximumSlotUpdates
	}

	// check if we can simply update the state or if a full refresh is required
	state = client.state.Load().(*mapping)
	state.missed++
	if state.missed < miss {
		state = &mapping{
			id:     state.id + 1,
			shards: true,
			nodes:  state.nodes,
			slots:  state.slots,
		}

		// update the slot in the new copy of the state
		state.slots[slot] = node

		client.state.Store(state)
		return
	}

	state, err = client.reconfigure(state, node)
	return
}

func (client *Client) redirect(request *Request) (state *mapping, node *Conn, err error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	state = client.state.Load().(*mapping)

	// already connected?
	if node = state.nodes[request.address]; node != nil {
		return
	}

	// connect to that new node then
	node = client.connect(request.address)

	state, err = client.reconfigure(state, node)
	return
}

func (client *Client) random() (node *Conn) {
	client.mu.Lock()
	defer client.mu.Unlock()

	// random walk
	n := rand.Intn(len(client.nodes))

	for _, conn := range client.nodes {
		if n == 0 {
			node = conn
			break
		}

		n--
	}

	return
}

func (client *Client) reconfigure(last *mapping, node *Conn) (next *mapping, err error) {
	result, err := node.Do("CLUSTER", "SLOTS")
	if err != nil {
		return
	}

	next = &mapping{
		id:     last.id + 1,
		shards: true,
		nodes:  make(map[string]*Conn),
	}

	// prepare the next state with only read access to the last state
	groups := result.([]interface{})
	for i := range groups {
		item := groups[i].([]interface{})
		a := item[0].(int64)
		b := item[1].(int64)
		m := item[2].([]interface{})
		addr := string(m[0].([]byte))
		port := m[1].(int64)
		name := fmt.Sprintf("tcp://%s:%d", addr, port)

		conn, ok := next.nodes[name]
		if !ok {
			conn, ok = last.nodes[name]
			if !ok {
				conn = client.connect(name)
			}

			next.nodes[name] = conn
		}

		// fill slots
		for j := a; j <= b; j++ {
			next.slots[j] = conn
		}
	}

	// update the client's references for random redirection and closing
	for name, item := range next.nodes {
		client.nodes[name] = item
	}

	client.state.Store(next)
	return
}

func init() {
	blueprint.Register(Client{})
}
