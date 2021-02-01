package rpc

import (
	"strings"
	"sync"

	"github.com/netdata/paho.golang/packets"
	"github.com/netdata/paho.golang/paho"
)

// MessageHandler is a type for a function that is invoked
// by a Router when it has received a Publish.
type MessageHandler func(*paho.Publish, func() error)

// Router is an interface of the functions for a struct that is
// used to handle invoking MessageHandlers depending on the
// the topic the message was published on.
// RegisterHandler() takes a string of the topic, and a MessageHandler
// to be invoked when Publishes are received that match that topic
// UnregisterHandler() takes a string of the topic to remove
// MessageHandlers for
// Route() takes a Publish message and determines which MessageHandlers
// should be invoked
type Router interface {
	paho.Router

	RegisterHandler(string, MessageHandler)
	UnregisterHandler(string)
}

// StandardRouter is a library provided implementation of a Router that
// allows for unique and multiple MessageHandlers per topic
type StandardRouter struct {
	sync.RWMutex
	subscriptions map[string][]MessageHandler
	aliases       map[uint16]string
}

// NewStandardRouter instantiates and returns an instance of a StandardRouter
func NewStandardRouter() *StandardRouter {
	return &StandardRouter{
		subscriptions: make(map[string][]MessageHandler),
		aliases:       make(map[uint16]string),
	}
}

// RegisterHandler is the library provided StandardRouter's
// implementation of the required interface function()
func (r *StandardRouter) RegisterHandler(topic string, h MessageHandler) {
	r.Lock()
	defer r.Unlock()
	r.subscriptions[topic] = append(r.subscriptions[topic], h)
}

// UnregisterHandler is the library provided StandardRouter's
// implementation of the required interface function()
func (r *StandardRouter) UnregisterHandler(topic string) {
	r.Lock()
	defer r.Unlock()
	delete(r.subscriptions, topic)
}

// Route is the library provided StandardRouter's implementation
// of the required interface function()
func (r *StandardRouter) Route(pb *packets.Publish, ack func() error) {
	r.RLock()
	defer r.RUnlock()

	m := PublishFromPacketPublish(pb)

	var topic string
	if pb.Properties.TopicAlias != nil {
		if pb.Topic != "" {
			//Register new alias
			r.aliases[*pb.Properties.TopicAlias] = pb.Topic
		}
		if t, ok := r.aliases[*pb.Properties.TopicAlias]; ok {
			topic = t
		}
	} else {
		topic = m.Topic
	}

	for route, handlers := range r.subscriptions {
		if match(route, topic) {
			for _, handler := range handlers {
				handler(m, ack)
			}
		}
	}
}

func match(route, topic string) bool {
	return route == topic || routeIncludesTopic(route, topic)
}

func matchDeep(route []string, topic []string) bool {
	if len(route) == 0 {
		return len(topic) == 0
	}

	if len(topic) == 0 {
		return route[0] == "#"
	}

	if route[0] == "#" {
		return true
	}

	if (route[0] == "+") || (route[0] == topic[0]) {
		return matchDeep(route[1:], topic[1:])
	}
	return false
}

func routeIncludesTopic(route, topic string) bool {
	return matchDeep(routeSplit(route), topicSplit(topic))
}

func routeSplit(route string) []string {
	if len(route) == 0 {
		return nil
	}
	var result []string
	if strings.HasPrefix(route, "$share") {
		result = strings.Split(route, "/")[1:]
	} else {
		result = strings.Split(route, "/")
	}
	return result
}

func topicSplit(topic string) []string {
	if len(topic) == 0 {
		return nil
	}
	return strings.Split(topic, "/")
}

// SingleHandlerRouter is a library provided implementation of a Router
// that stores only a single MessageHandler and invokes this MessageHandler
// for all received Publishes
type SingleHandlerRouter struct {
	sync.Mutex
	aliases map[uint16]string
	handler MessageHandler
}

// NewSingleHandlerRouter instantiates and returns an instance of a SingleHandlerRouter
func NewSingleHandlerRouter(h MessageHandler) *SingleHandlerRouter {
	return &SingleHandlerRouter{
		aliases: make(map[uint16]string),
		handler: h,
	}
}

// RegisterHandler is the library provided SingleHandlerRouter's
// implementation of the required interface function()
func (s *SingleHandlerRouter) RegisterHandler(topic string, h MessageHandler) {
	s.handler = h
}

// UnregisterHandler is the library provided SingleHandlerRouter's
// implementation of the required interface function()
func (s *SingleHandlerRouter) UnregisterHandler(topic string) {}

// Route is the library provided SingleHandlerRouter's
// implementation of the required interface function()
func (s *SingleHandlerRouter) Route(pb *packets.Publish, ack func() error) {
	m := PublishFromPacketPublish(pb)

	if pb.Properties.TopicAlias != nil {
		if pb.Topic != "" {
			//Register new alias
			s.aliases[*pb.Properties.TopicAlias] = pb.Topic
		}
		if t, ok := s.aliases[*pb.Properties.TopicAlias]; ok {
			m.Topic = t
		}
	}
	s.handler(m, ack)
}

// PublishFromPacketPublish takes a packets library Publish and
// returns a paho library Publish
func PublishFromPacketPublish(p *packets.Publish) *paho.Publish {
	v := &paho.Publish{
		QoS:     p.QoS,
		Retain:  p.Retain,
		Topic:   p.Topic,
		Payload: p.Payload,
	}
	v.InitProperties(p.Properties)

	return v
}
