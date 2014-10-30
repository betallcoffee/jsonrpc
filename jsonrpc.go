// jsonrpc
package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	log "llog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var (
	// Precompute the reflect.Type of error
	typeOfError = reflect.TypeOf((*RPCError)(nil))
	null        = json.RawMessage([]byte("null"))
)

const (
	PARSEERROR     = -32700
	INVALIDREQUEST = -32600
	METHODNOTFOUND = -32601
	INVALIDPARAMS  = -32602
	INTERNALERROR  = -32603
)

type service struct {
	name     string
	rcvr     reflect.Value
	rcvrType reflect.Type
	methods  map[string]*serviceMethod
}

type serviceMethod struct {
	method   reflect.Method
	argsType reflect.Type
	resType  reflect.Type
}

type JSONRPC struct {
	mutex       sync.Mutex
	services    map[string]*service
	typeOfFirst reflect.Type
}

func NewJSONRPC(typeOfFirst reflect.Type) *JSONRPC {
	rpc := &JSONRPC{
		typeOfFirst: typeOfFirst,
	}
	return rpc
}

func (self *JSONRPC) Register(receiver interface{}, name string) error {
	log.Trace("Register begin")
	defer log.Trace("Register end")
	srvc := &service{
		name:     name,
		rcvr:     reflect.ValueOf(receiver),
		rcvrType: reflect.TypeOf(receiver),
		methods:  make(map[string]*serviceMethod),
	}
	if name == "" {
		srvc.name = reflect.Indirect(srvc.rcvr).Type().Name()
	}

	for i := 0; i < srvc.rcvrType.NumMethod(); i++ {
		method := srvc.rcvrType.Method(i)
		log.Debug(method)
		mtType := method.Type

		if mtType.PkgPath() != "" {
			log.Trace("register: the method is not exported")
			continue
		}
		if mtType.NumIn() != 4 {
			log.Trace("register: the number of argument must be 4")
			continue
		}
		reqType := mtType.In(1)
		if reqType.Kind() != reflect.Ptr || reqType.Elem() != self.typeOfFirst {
			log.Trace("register: the type of first argument error")
			continue
		}
		argsType := mtType.In(2)
		if argsType.Kind() != reflect.Ptr || !isExportedOrBuiltin(argsType) {
			log.Trace("register: the type of second argument must be point and exported")
			continue
		}
		resType := mtType.In(3)
		if resType.Kind() != reflect.Ptr || !isExportedOrBuiltin(resType) {
			log.Trace("register: the type of third argument must be point and exported")
			continue
		}

		if mtType.NumOut() != 1 {
			log.Trace("register: the number of return must be 1")
			continue
		}
		if returnType := mtType.Out(0); returnType != typeOfError {
			log.Trace("register: the type of return must be RPCError")
			continue
		}
		srvc.methods[method.Name] = &serviceMethod{
			method:   method,
			argsType: argsType.Elem(),
			resType:  resType.Elem(),
		}
	}
	// Add to the map.
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.services == nil {
		self.services = make(map[string]*service)
	} else if _, ok := self.services[srvc.name]; ok {
		return fmt.Errorf("rpc: service already defined: %q", srvc.name)
	}
	self.services[srvc.name] = srvc
	log.Trace(*self.services[srvc.name])
	return nil
}

func (self *JSONRPC) get(method string) (*service, *serviceMethod, error) {
	log.Trace("get begin")
	defer log.Trace("get end")
	log.Debug(method)
	parts := strings.Split(method, ".")
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("JSONRPC: service/method request ill-formed: %q", method)
	}
	self.mutex.Lock()
	service := self.services[parts[0]]
	self.mutex.Unlock()
	if service == nil {
		err := fmt.Errorf("JSONRPC: can't find service %q", method)
		return nil, nil, err
	}
	serviceMethod := service.methods[parts[1]]
	if serviceMethod == nil {
		err := fmt.Errorf("JSONRPC: can't find method %q", method)
		return nil, nil, err
	}
	log.Debug(service)
	log.Debug(serviceMethod)
	return service, serviceMethod, nil
}

func (self *JSONRPC) Dispatch(first interface{}, req *request) []byte {
	log.Trace("JSONRPCDispath begin")
	defer log.Trace("JSONRPCDispath end")

	service, serviceMethod, err := self.get(req.Method)
	if err != nil {
		log.Error(err)
		return NewResponse(nil, NewRPCError(METHODNOTFOUND, req.Method+" not found"))
	}
	args := reflect.New(serviceMethod.argsType)
	if err := req.readParams(args.Interface()); err != nil {
		log.Error(err)
		return NewResponse(nil, NewRPCError(INVALIDPARAMS, req.Method+" invalid params "+string(*req.Params)))
	}
	res := reflect.New(serviceMethod.resType)
	var errValue []reflect.Value

	errValue = serviceMethod.method.Func.Call([]reflect.Value{
	service.rcvr,
	reflect.ValueOf(first),
	args,
	res,
})
	// Cast the result to error if needed.
	var errResult *RPCError
	errInter := errValue[0].Interface()
	if errInter != nil {
		errResult = errInter.(*RPCError)
		log.Error(errResult)
	}
	return NewResponse(res.Interface(), errResult)
}

type request struct {
	Method string           `json:"method"`
	Params *json.RawMessage `json:"params"`
	ID     int32            `json:"id"`
}

type response struct {
	Result interface{} `json:"result"`
	Error  *RPCError `json:"error"`
	ID     int32       `json:"id"`
}

type RPCError struct {
	Code    int32 `json:"code"`
	Message string `json:"message"`
}

func NewRPCError(code int32, message string) *RPCError {
	return &RPCError{
		Code: code,
		Message: message,
	}
}

func (e RPCError) Error() string {
	res, err := json.Marshal(e)
	if err != nil {
		return string(err.Error())
	}
	return string(res)
}

func NewResponse(result interface{}, e *RPCError) ([]byte) {
	log.Trace("NewResponse begin")
	defer log.Trace("NewResponse end")
	res := &response{
		Result: result,
		Error:  nil,
	}
	if e != nil {
		res.Error = e
		res.Result = nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		log.Error(err)
		return []byte(err.Error())
	}
	log.Debugf(string(b))
	return b
}

func NewRequestFromHttp(r *http.Request) (*request, *RPCError) {
	log.Trace("NewRequestHttp begin")
	defer log.Trace("NewRequestHttp end")
	req := new(request)
	err := json.NewDecoder(r.Body).Decode(req)
	r.Body.Close()
	if err != nil {
		log.Error(err)
		return nil, NewRPCError(INVALIDREQUEST, r.RemoteAddr+"the request is invlide:"+err.Error())
	}
	return req, nil
}

func NewRequestFromData(data []byte) (*request, *RPCError) {
	log.Trace("NewRequestData begin")
	defer log.Trace("NewRequestData end")
	req := new(request)
	err := json.Unmarshal(data, req)
	if err != nil {
		log.Error(err)
		return nil, NewRPCError(INVALIDREQUEST, "the request is invlide:"+err.Error())
	}
	return req, nil
}

func (req *request) readParams(arg interface{}) error {
	log.Trace("readParams begin")
	defer log.Trace("readParams end")
	if req.Params == nil {
		return errors.New("JSONRPC method request ill-formed: missing params field")
	} else {
		params := [1]interface{}{arg}
		err := json.Unmarshal(*req.Params, &params)
		log.Debugf("%v", arg)
		return err
	}
}

// isExported returns true of a string is an exported (upper case) name.
func isExported(name string) bool {
	rune, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(rune)
}

// isExportedOrBuiltin returns true if a type is exported or a builtin.
func isExportedOrBuiltin(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return isExported(t.Name()) || t.PkgPath() == ""
}
