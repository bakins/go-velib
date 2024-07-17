package velib

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"

	dbus "github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

type Service struct {
	// lock for values
	mu             sync.Mutex
	conn           *dbus.Conn
	name           string
	values         map[string]ServiceValue
	deviceInstance int
	deviceName     string
	deviceClass    string
}

var nonAlphanumberic = regexp.MustCompile("[^a-zA-Z0-9]+")

// TODO: validate name
func NewService(conn *dbus.Conn, name string) (*Service, error) {
	parts := strings.Split(name, ".")
	if len(parts) < 3 {
		return nil, fmt.Errorf("name %q must have at least 3 parts", name)
	}

	deviceName := parts[len(parts)-1]
	deviceName = nonAlphanumberic.ReplaceAllString(deviceName, "_")
	deviceName = strings.ToLower(deviceName)

	deviceClass := parts[len(parts)-2]

	name = strings.Join(parts[:len(parts)-1], ".") + "." + deviceName

	s := &Service{
		conn:           conn,
		name:           name,
		values:         make(map[string]ServiceValue),
		deviceName:     deviceName,
		deviceClass:    deviceClass,
		deviceInstance: -1,
	}

	return s, nil
}

func (s *Service) Close() error {
	reply, err := s.conn.ReleaseName(s.name)
	if err != nil {
		return fmt.Errorf("failed to release name %s: %w", s.name, err)
	}

	if reply != dbus.ReleaseNameReplyReleased {
		return fmt.Errorf("failed to release name %v: %d", s.name, reply)
	}

	return nil
}

func (s *Service) GetDeviceInstance() (int, error) {
	if s.deviceInstance != -1 {
		return s.deviceInstance, nil
	}

	getDeviceInstance := func() (int, error) {
		obj := s.conn.Object("com.victronenergy.settings",
			dbus.ObjectPath("/Settings/Devices/"+s.deviceName+"/ClassAndVrmInstance"))

		var value string
		if err := obj.Call("GetValue", 0).Store(&value); err != nil {
			return 0, fmt.Errorf("failed to get value: %w", err)
		}

		parts := strings.Split(value, ":")

		if len(parts) != 2 {
			return 0, fmt.Errorf("unexpected value %q", value)
		}

		return strconv.Atoi(parts[1])
	}

	deviceInstance, err := getDeviceInstance()
	if err != nil {
		deviceInstance = 1
	}

	// See https://github.com/victronenergy/localsettings?tab=readme-ov-file#using-addsetting-to-allocate-a-vrm-device-instance
	var result int
	err = s.conn.Object("com.victronenergy.settings", "/Settings/Devices").Call(
		"AddSetting",
		0,
		s.deviceName,          // group
		"ClassAndVrmInstance", // name
		dbus.MakeVariant(fmt.Sprintf("%s:%d", s.deviceClass, deviceInstance)), // defaultValue
		"s",                  // itemType
		dbus.MakeVariant(""), // minimum
		dbus.MakeVariant(""), // maximum
	).Store(&result)
	if err != nil {
		return -1, fmt.Errorf("failed to store result: %w", err)
	}

	if result != 0 {
		return -1, fmt.Errorf("unexpected result %d", result)
	}

	deviceInstance, err = getDeviceInstance()
	if err != nil {
		return -1, fmt.Errorf("failed to get device instance: %w", err)
	}

	return deviceInstance, nil
}

func (s *Service) Register() error {
	obj := s.conn.Object(s.name, dbus.ObjectPath("/"))

	w := &dbusServiceWrapper{service: s}

	if err := s.conn.ExportAll(
		w,
		obj.Path(),
		"com.victronenergy.BusItem",
	); err != nil {
		return fmt.Errorf("failed to export object: %w", err)
	}

	node := &introspect.Node{}
	node.Name = "com.victronenergy.BusItem"
	iface := &introspect.Interface{}
	iface.Name = "com.victronenergy.BusItem"
	iface.Methods = introspect.Methods(w)
	node.Interfaces = append(node.Interfaces, *iface)
	dbusXMLinsp := introspect.NewIntrospectable(node)

	if err := s.conn.Export(
		dbusXMLinsp,
		obj.Path(),
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}

	reply, err := s.conn.RequestName(s.name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("failed to request name: %w", err)
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("name %q already taken", s.name)
	}

	return nil
}

type ServiceValue interface {
	GetValue() (any, error)
	GetText() (string, error)
	SetValue(value any) error
}

// TODO allow setting without notifying?
func (s *Service) AddPath(path string, value any) (ServiceValue, error) {
	base := baseValue{
		service: s,
		path:    path,
	}

	switch v := value.(type) {
	case *FormatterValue:
		base.value = v
	default:
		val, ok := v.(ServiceValue)
		if ok {
			base.value = val
		} else {
			base.value = &anyValue{
				value: v,
			}
		}
	}

	wrapper := &valueWrapper{
		base: &base,
	}

	fmt.Printf("AddPath %s %s %T\n", s.name, path, wrapper.base.value)

	obj := s.conn.Object(s.name, dbus.ObjectPath(path))

	err := s.conn.ExportAll(
		wrapper.base,
		obj.Path(),
		"com.victronenergy.BusItem",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to export service value: %w", err)
	}

	node := &introspect.Node{}
	node.Name = "com.victronenergy.BusItem"
	iface := &introspect.Interface{}
	iface.Name = "com.victronenergy.BusItem"
	iface.Methods = introspect.Methods(wrapper.base)
	node.Interfaces = append(node.Interfaces, *iface)
	dbusXMLinsp := introspect.NewIntrospectable(node)

	err = s.conn.Export(
		dbusXMLinsp,
		obj.Path(),
		"org.freedesktop.DBus.Introspectable")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.values[path] = wrapper
	s.mu.Unlock()

	wrapper.base.mu.Lock()
	defer wrapper.base.mu.Unlock()

	current, err := wrapper.base.value.GetValue()
	if err != nil {
		return nil, fmt.Errorf("failed to get current value: %w", err)
	}

	if err := wrapper.base.value.SetValue(current); err != nil {
		return nil, fmt.Errorf("failed to set initial value: %w", err)
	}
	return wrapper, nil
}

func wrapError(err error) *dbus.Error {
	if err == nil {
		return nil
	}

	switch t := err.(type) {
	case *dbus.Error:
		return t
	default:
		return &dbus.Error{
			Name: "com.victronenergy.BusItem.Error",
			Body: []interface{}{err.Error()},
		}
	}
}

type baseValue struct {
	mu      sync.Mutex
	path    string
	service *Service
	value   ServiceValue
}

type valueWrapper struct {
	base *baseValue
}

func (w *valueWrapper) SetValue(value any) error {
	_, err := w.base.SetValue(value)
	if err != nil {
		return err
	}

	return nil
}

func (w *valueWrapper) GetValue() (any, error) {
	val, err := w.base.GetValue()
	if err != nil {
		return nil, err
	}

	return val, nil
}

func (w *valueWrapper) GetText() (string, error) {
	val, err := w.base.GetText()
	if err != nil {
		return "", err
	}

	return val, nil
}

func (b *baseValue) SetValue(value any) (int, *dbus.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.value.SetValue(value); err != nil {
		return -1, wrapError(err)
	}

	text, err := b.value.GetText()
	if err != nil {
		return -1, wrapError(err)
	}

	obj := b.service.conn.Object(b.service.name, dbus.ObjectPath(b.path))

	if err := b.service.conn.Emit(
		obj.Path(),
		"com.victronenergy.BusItem.PropertiesChanged",
		map[string]any{
			"Value": value,
			"Text":  text,
		},
	); err != nil {
		return -1, wrapError(err)
	}

	return 0, nil
}

func (b *baseValue) GetValue() (any, *dbus.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	value, err := b.value.GetValue()
	if err != nil {
		return nil, wrapError(err)
	}

	return value, nil
}

func (b *baseValue) GetText() (string, *dbus.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	text, err := b.value.GetText()
	if err != nil {
		return "", wrapError(err)
	}

	return text, nil
}

type FormatterValue struct {
	formatter func(any) string
	value     any
}

func NewFormatterObject(value any, formatter func(any) string) *FormatterValue {
	if formatter == nil {
		formatter = func(a any) string {
			return fmt.Sprintf("%v", a)
		}
	}

	return &FormatterValue{
		formatter: formatter,
		value:     value,
	}
}

func (f *FormatterValue) SetValue(value any) error {
	f.value = value

	return nil
}

func (f *FormatterValue) GetValue() (any, error) {
	return f.value, nil
}

func (f *FormatterValue) GetText() (string, error) {
	return f.formatter(f.value), nil
}

func (s *Service) ItemsChanged() *dbus.Error {
	return nil
}

type dbusServiceWrapper struct {
	service *Service
}

func (s *dbusServiceWrapper) ItemsChanged() *dbus.Error {
	return nil
}

// dbus signature a{sa{sv}}
func (s *dbusServiceWrapper) GetItems() (map[string]map[string]any, *dbus.Error) {
	out := make(map[string]map[string]any)

	s.service.mu.Lock()
	values := maps.Clone(s.service.values)
	s.service.mu.Unlock()

	for path, item := range values {

		val, err := item.GetValue()
		if err != nil {
			return nil, wrapError(err)
		}

		text, err := item.GetText()
		if err != nil {
			return nil, wrapError(err)
		}

		out[path] = map[string]any{
			"Value": val,
			"Text":  text,
		}
	}

	return out, nil
}

type anyValue struct {
	value any
}

func (v *anyValue) SetValue(value any) error {
	v.value = value

	return nil
}

func (v *anyValue) GetValue() (any, error) {
	return v.value, nil
}

func (v *anyValue) GetText() (string, error) {
	return fmt.Sprintf("%v", v.value), nil
}
