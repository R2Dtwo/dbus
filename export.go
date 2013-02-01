package dbus

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"reflect"
)

var (
	errmsgInvalidArg = Error{
		"org.freedesktop.DBus.Error.InvalidArgs",
		[]interface{}{"Invalid type / number of args"},
	}
	errmsgUnknownMethod = Error{
		"org.freedesktop.DBus.Error.UnknownMethod",
		[]interface{}{"Unkown / invalid method"},
	}
)

type expObject struct {
	intro string
	interfaces map[string]interface{}
}

func (conn *Connection) handleCall(msg *Message) {
	var vs []interface{}
	if len(msg.Body) != 0 {
		vs = msg.Headers[FieldSignature].value.(Signature).Values()
		dec := NewDecoder(bytes.NewBuffer(msg.Body), msg.Order)
		err := dec.DecodeMulti(vs...)
		if err != nil {
			return
		}
		vs = dereferenceAll(vs)
	}
	name := msg.Headers[FieldMember].value.(string)
	path := msg.Headers[FieldPath].value.(ObjectPath)
	ifacename := msg.Headers[FieldInterface].value.(string)
	sender := msg.Headers[FieldSender].value.(string)
	serial := msg.Serial
	conn.handlersLck.RLock()
	obj, ok := conn.handlers[path]
	if !ok {
		conn.sendError(errmsgUnknownMethod, sender, serial)
		conn.handlersLck.RUnlock()
		return
	}
	iface := obj.interfaces[ifacename]
	conn.handlersLck.RUnlock()
	if ifacename == "org.freedesktop.DBus.Peer" {
		switch name {
		case "Ping":
			conn.sendReply(sender, serial)
		case "GetMachineId":
			conn.sendReply(sender, serial, conn.uuid)
		}
		return
	} else if ifacename == "org.freedesktop.DBus.Introspectable" && name == "Introspect" {
		if intro := obj.intro; intro != "" {
			conn.sendReply(sender, serial, intro)
		} else {
			conn.sendError(errmsgUnknownMethod, sender, serial)
		}
		return
	}
	if iface == nil {
		conn.sendError(errmsgUnknownMethod, sender, serial)
		return
	}
	m := reflect.ValueOf(iface).MethodByName(name)
	if !m.IsValid() {
		conn.sendError(errmsgUnknownMethod, sender, serial)
		return
	}
	t := m.Type()
	if t.NumOut() == 0 ||
		t.Out(t.NumOut()-1) != reflect.TypeOf(&errmsgInvalidArg) {

		conn.sendError(errmsgUnknownMethod, sender, serial)
		return
	}
	if t.NumIn() != len(vs) {
		conn.sendError(errmsgInvalidArg, sender, serial)
		return
	}
	for i := 0; i < t.NumIn(); i++ {
		if t.In(i) != reflect.TypeOf(vs[i]) {
			conn.sendError(errmsgInvalidArg, sender, serial)
			return
		}
	}
	params := make([]reflect.Value, len(vs))
	for i := 0; i < len(vs); i++ {
		params[i] = reflect.ValueOf(vs[i])
	}
	ret := m.Call(params)
	if em := ret[t.NumOut()-1].Interface().(*Error); em != nil {
		conn.sendError(*em, sender, serial)
		return
	}
	if msg.Flags&FlagNoReplyExpected == 0 {
		body := new(bytes.Buffer)
		sig := ""
		enc := NewEncoder(body, binary.LittleEndian)
		for i := 0; i < len(ret)-1; i++ {
			enc.encode(ret[i])
			sig += getSignature(ret[i].Type())
		}
		reply := new(Message)
		reply.Order = binary.LittleEndian
		reply.Type = TypeMethodReply
		reply.Serial = <-conn.serial
		reply.Headers = make(map[HeaderField]Variant)
		reply.Headers[FieldDestination] = msg.Headers[FieldSender]
		reply.Headers[FieldReplySerial] = MakeVariant(msg.Serial)
		if len(ret) != 1 {
			reply.Headers[FieldSignature] = MakeVariant(Signature{sig})
			reply.Body = body.Bytes()
		} else {
			reply.Body = []byte{}
		}
		conn.out <- reply
	}
}

// Emit emits the given signal on the message bus.
func (conn *Connection) Emit(path ObjectPath, iface string, name string, values ...interface{}) {
	msg := new(Message)
	msg.Order = binary.LittleEndian
	msg.Type = TypeSignal
	msg.Serial = <-conn.serial
	msg.Headers = make(map[HeaderField]Variant)
	msg.Headers[FieldInterface] = MakeVariant(iface)
	msg.Headers[FieldMember] = MakeVariant(name)
	msg.Headers[FieldPath] = MakeVariant(path)
	if len(values) > 0 {
		msg.Headers[FieldSignature] = MakeVariant(GetSignature(values...))
		buf := new(bytes.Buffer)
		enc := NewEncoder(buf, binary.LittleEndian)
		enc.EncodeMulti(values...)
		msg.Body = buf.Bytes()
	} else {
		msg.Body = []byte{}
	}
	conn.out <- msg
}

// Export the given value as an object on the message bus.
//
// If a method call on the given path and interface is received, an exported
// method with the same name is called if the parameters match and the last
// return value is of type *ErrorMessage. If this value is not nil, it is
// sent back to the caller as an error. Otherwise, a method reply is sent
// with the other parameters as its body.
//
// The method is executed in a new goroutine.
//
// If you need to implement multiple interfaces on one "object", wrap it with
// (Go) interfaces.
//
// If path is not a valid object path, Export panics.
func (conn *Connection) Export(v interface{}, path ObjectPath, iface string) {
	if !path.IsValid() {
		panic("(*dbus.Connection).Export: invalid path name")
	}
	conn.handlersLck.Lock()
	if _, ok := conn.handlers[path]; !ok {
		conn.handlers[path] = new(expObject)
		conn.handlers[path].interfaces = make(map[string]interface{})
	}
	conn.handlers[path].interfaces[iface] = v
	conn.handlersLck.Unlock()
}

// ReleaseName calls org.freedesktop.DBus.ReleaseName. You should use only this
// method to release a name (see below).
func (conn *Connection) ReleaseName(name string) (ReleaseNameReply, error) {
	var r uint32
	err := conn.busObj.Call("org.freedesktop.DBus.ReleaseName", 0, name).Store(&r)
	if err != nil {
		return 0, err
	}
	if r == uint32(ReleaseNameReplyReleased) {
		for i, v := range conn.names {
			if v == name {
				copy(conn.names[i:], conn.names[i+1:])
				conn.names = conn.names[:len(conn.names)-1]
			}
		}
	}
	return ReleaseNameReply(r), nil
}

// RequestName calls org.freedesktop.DBus.RequestName. You should use only this
// method to request a name because package dbus needs to keep track of all
// names that the connection has.
func (conn *Connection) RequestName(name string, flags RequestNameFlags) (RequestNameReply, error) {
	var r uint32
	err := conn.busObj.Call("org.freedesktop.DBus.RequestName", 0, name, flags).Store(&r)
	if err != nil {
		return 0, err
	}
	if r == uint32(RequestNameReplyPrimaryOwner) {
		conn.names = append(conn.names, name)
	}
	return RequestNameReply(r), nil
}

// SetIntrospect sets the introspection data that is returned if a peer calls
// org.freedesktop.Introspectable.Introspect on the given object path. If the
// string is "", an error is returned to peers that try to call Introspect.
//
// An error is returned if the given string is not valid introspection data.
func (conn *Connection) SetIntrospect(path ObjectPath, intro string) error {
	var n Node
	if err := xml.Unmarshal([]byte(intro), &n); err != nil {
		return err
	}
	// TODO: check that n is valid
	conn.handlersLck.Lock()
	if _, ok := conn.handlers[path]; !ok {
		conn.handlers[path] = new(expObject)
		conn.handlers[path].interfaces = make(map[string]interface{})
	}
	conn.handlers[path].intro = intro
	conn.handlersLck.Unlock()
	return nil
}

// ReleaseNameReply is the reply to a ReleaseName call.
type ReleaseNameReply uint32

const (
	ReleaseNameReplyReleased ReleaseNameReply = 1 + iota
	ReleaseNameReplyNonExistent
	ReleaseNameReplyNotOwner
)

// RequestNameFlags represents the possible flags for the RequestName call.
type RequestNameFlags uint32

const (
	NameFlagAllowReplacement RequestNameFlags = 1 << iota
	NameFlagReplaceExisting
	NameFlagDoNotQueue
)

// RequestNameReply is the reply to a RequestName call.
type RequestNameReply uint32

const (
	RequestNameReplyPrimaryOwner RequestNameReply = 1 + iota
	RequestNameReplyInQueue
	RequestNameReplyExists
	RequestNameReplyAlreadyOwner
)
