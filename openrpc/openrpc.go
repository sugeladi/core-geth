// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package openrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/jsonschema"
	"github.com/davecgh/go-spew/spew"
	"github.com/etclabscore/go-jsonschema-traverse"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/go-openapi/spec"
	goopenrpcT "github.com/gregdhill/go-openrpc/types"
)


func (s *rpc.Server) OpenRPCInfo() goopenrpcT.Info {
	return goopenrpcT.Info{
		Title:          "Ethereum JSON-RPC",
		Description:    "This API lets you interact with an EVM-based client via JSON-RPC",
		TermsOfService: "https://github.com/etclabscore/core-geth/blob/master/COPYING",
		Contact: goopenrpcT.Contact{
			Name:  "",
			URL:   "",
			Email: "",
		},
		License: goopenrpcT.License{
			Name: "Apache-2.0",
			URL:  "https://www.apache.org/licenses/LICENSE-2.0.html",
		},
		Version: "1.0.10",
	}
}

func (s *rpc.Server) OpenRPCExternalDocs() goopenrpcT.ExternalDocs {
	return goopenrpcT.ExternalDocs{
		Description: "Source",
		URL:         "https://github.com/etclabscore/core-geth",
	}
}

func (s *rpc.RPCService) SetOpenRPCDiscoverDocument(documentPath string) error {
	bs, err := ioutil.ReadFile(documentPath)
	if err != nil {
		return err
	}
	doc := string(bs)
	return s.server.SetOpenRPCSchemaRaw(doc)
}


type MutateType string

const (
	SchemaMutateType_Expand            = "schema_expand"
	SchemaMutateType_RemoveDefinitions = "schema_remove_definitions"
)

type DocumentDiscoverOpts struct {
	Inline          bool
	SchemaMutations []MutateType
	MethodBlackList []string
}

type ServerProvider interface {
	Methods() map[string][]reflect.Value
	OpenRPCInfo() goopenrpcT.Info
	OpenRPCExternalDocs() goopenrpcT.ExternalDocs
}

type Document struct {
	serverProvider ServerProvider
	discoverOpts   *DocumentDiscoverOpts
	spec1          *goopenrpcT.OpenRPCSpec1
}

func (d *Document) Document() *goopenrpcT.OpenRPCSpec1 {
	return d.spec1
}

func Wrap(serverProvider ServerProvider, opts *DocumentDiscoverOpts) *Document {
	if serverProvider == nil {
		panic("openrpc-wrap-nil-serverprovider")
	}
	return &Document{serverProvider: serverProvider, discoverOpts: opts}
}

func isDiscoverMethodBlacklisted(d *DocumentDiscoverOpts, name string) bool {
	if d != nil && len(d.MethodBlackList) > 0 {
		for _, b := range d.MethodBlackList {
			if regexp.MustCompile(b).MatchString(name) {
				return true
			}
		}
	}
	return false
}

func (d *Document) Discover() (doc *goopenrpcT.OpenRPCSpec1, err error) {
	if d == nil || d.serverProvider == nil {
		return nil, errors.New("server provider undefined")
	}

	// TODO: Caching?

	d.spec1 = NewSpec()
	d.spec1.Info = d.serverProvider.OpenRPCInfo()
	d.spec1.ExternalDocs = d.serverProvider.OpenRPCExternalDocs()

	// Set version by runtime, after parse.
	spl := strings.Split(d.spec1.Info.Version, "+")
	d.spec1.Info.Version = fmt.Sprintf("%s-%s-%d", spl[0], time.Now().Format(time.RFC3339), time.Now().Unix())

	d.spec1.Methods = []goopenrpcT.Method{}
	mets := d.serverProvider.Methods()

	for k, rvals := range mets {
		if rvals == nil || len(rvals) == 0 {
			fmt.Println("skip bad k", k)
			continue
		}

		if isDiscoverMethodBlacklisted(d.discoverOpts, k) {
			continue
		}

		m, err := d.GetMethod(k, rvals)
		if err != nil {
			return nil, err
		}
		d.spec1.Methods = append(d.spec1.Methods, *m)
	}
	sort.Slice(d.spec1.Methods, func(i, j int) bool {
		return d.spec1.Methods[i].Name < d.spec1.Methods[j].Name
	})

	if d.discoverOpts != nil && len(d.discoverOpts.SchemaMutations) > 0 {
		for _, mutation := range d.discoverOpts.SchemaMutations {
			d.documentRunSchemasMutation(mutation)
		}
	}
	//if d.discoverOpts != nil && !d.discoverOpts.Inline {
	//	d.spec1.Components.Schemas = make(map[string]spec.Schema)
	//}

	// TODO: Flatten/Inline ContentDescriptors and Schemas

	return d.spec1, nil
}

func removeDefinitionsFieldSchemaMutation(s *spec.Schema) error {
	s.Definitions = nil
	return nil
}

func expandSchemaMutation(s *spec.Schema) error {
	return spec.ExpandSchema(s, s, nil)
}

func (d *Document) documentSchemaMutation(mut func(s *spec.Schema) error) {
	a := go_jsonschema_traverse.NewAnalysisT()
	for i := 0; i < len(d.spec1.Methods); i++ {

		met := d.spec1.Methods[i]

		// Params.
		for ip := 0; ip < len(met.Params); ip++ {
			par := met.Params[ip]
			a.WalkDepthFirst(&par.Schema, mut)
			met.Params[ip] = par
		}

		// Result (single).
		a.WalkDepthFirst(&met.Result.Schema, mut)
	}
	for k := range d.spec1.Components.ContentDescriptors {
		cd := d.spec1.Components.ContentDescriptors[k]
		a.WalkDepthFirst(&cd.Schema, mut)
		d.spec1.Components.ContentDescriptors[k] = cd
	}
	for k := range d.spec1.Components.Schemas {
		s := d.spec1.Components.Schemas[k]
		a.WalkDepthFirst(&s, mut)
		d.spec1.Components.Schemas[k] = s
	}
}
func (d *Document) documentRunSchemasMutation(id MutateType) {
	switch id {
	case SchemaMutateType_Expand:
		d.documentSchemaMutation(expandSchemaMutation)
	case SchemaMutateType_RemoveDefinitions:
		d.documentSchemaMutation(removeDefinitionsFieldSchemaMutation)
	}
}

func documentGetAstFunc(rcvr reflect.Value, fn reflect.Value, astFile *ast.File, rf *runtime.Func) *ast.FuncDecl {
	rfName := runtimeFuncName(rf)
	for _, decl := range astFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name == nil || fn.Name.Name != rfName {
			continue
		}
		fnRecName := ""
		for _, l := range fn.Recv.List {
			if fnRecName != "" {
				break
			}
			i, ok := l.Type.(*ast.Ident)
			if ok {
				fnRecName = i.Name
				continue
			}
			s, ok := l.Type.(*ast.StarExpr)
			if ok {
				fnRecName = fmt.Sprintf("%v", s.X)
			}
		}

		if rcvr.IsValid() && !rcvr.IsNil() {
			reRec := regexp.MustCompile(fnRecName + `\s`)
			if !reRec.MatchString(rcvr.String()) {
				continue
			}
		}
		return fn
	}
	return nil
}

func (d *Document) GetMethod(name string, fns []reflect.Value) (*goopenrpcT.Method, error) {
	var recvr reflect.Value
	var fn reflect.Value

	if len(fns) == 2 && fns[0].IsValid() && fns[1].IsValid() {
		recvr, fn = fns[0], fns[1]
	} else if len(fns) == 1 {
		fn = fns[0]
	}

	rtFunc := runtime.FuncForPC(fn.Pointer())
	cbFile, _ := rtFunc.FileLine(rtFunc.Entry())

	tokenFileSet := token.NewFileSet()
	astFile, err := parser.ParseFile(tokenFileSet, cbFile, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	astFuncDecl := documentGetAstFunc(recvr, fn, astFile, rtFunc)

	if astFuncDecl == nil {
		return nil, fmt.Errorf("nil ast func: method name: %s", name)
	}

	method, err := documentMakeMethod(name, recvr, fn, rtFunc, astFuncDecl)
	if err != nil {
		return nil, fmt.Errorf("make method error method=%s cb=%s error=%v", name, spew.Sdump(fn), err)
	}
	return &method, nil
}

func NewSpec() *goopenrpcT.OpenRPCSpec1 {
	return &goopenrpcT.OpenRPCSpec1{
		OpenRPC: "1.2.4",
		Info:    goopenrpcT.Info{},
		Servers: []goopenrpcT.Server{},
		Methods: []goopenrpcT.Method{},
		Components: goopenrpcT.Components{
			ContentDescriptors:    make(map[string]*goopenrpcT.ContentDescriptor),
			Schemas:               make(map[string]spec.Schema),
			Examples:              make(map[string]goopenrpcT.Example),
			Links:                 make(map[string]goopenrpcT.Link),
			Errors:                make(map[string]goopenrpcT.Error),
			ExamplePairingObjects: make(map[string]goopenrpcT.ExamplePairing),
			Tags:                  make(map[string]goopenrpcT.Tag),
		},
		ExternalDocs: goopenrpcT.ExternalDocs{},
		Objects:      goopenrpcT.NewObjectMap(),
	}
}

func documentGetArgTypes(rcvr, val reflect.Value) (argTypes []reflect.Type) {
	fntype := val.Type()
	// Skip receiver and context.Context parameter (if present).
	firstArg := 0
	if rcvr.IsValid() && !rcvr.IsNil() {
		firstArg++
	}
	if fntype.NumIn() > firstArg && fntype.In(firstArg) == rpc.contextType {
		firstArg++
	}
	// Add all remaining parameters.
	argTypes = make([]reflect.Type, fntype.NumIn()-firstArg)
	for i := firstArg; i < fntype.NumIn(); i++ {
		argTypes[i-firstArg] = fntype.In(i)
	}
	return
}
func documentGetRetTypes(val reflect.Value) (retTypes []reflect.Type) {
	fntype := val.Type()
	// Add all remaining parameters.
	retTypes = make([]reflect.Type, fntype.NumOut())
	for i := 0; i < fntype.NumOut(); i++ {
		retTypes[i] = fntype.Out(i)
	}
	return
}

func documentValHasContext(rcvr reflect.Value, val reflect.Value) bool {
	fntype := val.Type()
	// Skip receiver and context.Context parameter (if present).
	firstArg := 0
	if rcvr.IsValid() && !rcvr.IsNil() {
		firstArg++
	}
	return fntype.NumIn() > firstArg && fntype.In(firstArg) == rpc.contextType
}

func documentMakeMethod(name string, rcvr reflect.Value, cb reflect.Value, rt *runtime.Func, fn *ast.FuncDecl) (goopenrpcT.Method, error) {
	file, line := rt.FileLine(rt.Entry())

	m := goopenrpcT.Method{
		Name:    name,
		Tags:    []goopenrpcT.Tag{},
		Summary: fn.Doc.Text(),
		//Description: fmt.Sprintf("```\n%s\n```", string(buf.Bytes())), // rt.Name(),
		//  fmt.Sprintf("`%s`\n> [%s:%d][file://%s]", rt.Name(), file, line, file),
		//Description: "some words",
		ExternalDocs: goopenrpcT.ExternalDocs{
			Description: rt.Name(),
			URL:         fmt.Sprintf("file://%s:%d", file, line),
		},
		Params:         []*goopenrpcT.ContentDescriptor{},
		Result:         &goopenrpcT.ContentDescriptor{},
		Deprecated:     false,
		Servers:        []goopenrpcT.Server{},
		Errors:         []goopenrpcT.Error{},
		Links:          []goopenrpcT.Link{},
		ParamStructure: "by-position",
		Examples:       []goopenrpcT.ExamplePairing{},
	}

	defer func() {
		if m.Result.Name == "" {
			m.Result.Name = "null"
			m.Result.Schema.Type = []string{"null"}
			m.Result.Schema.Description = "Null"
		}
	}()

	argTypes := documentGetArgTypes(rcvr, cb)
	if fn.Type.Params != nil {
		j := 0
		for _, field := range fn.Type.Params.List {
			if field == nil {
				continue
			}
			if documentValHasContext(rcvr, cb) && strings.Contains(fmt.Sprintf("%s", field.Type), "context") {
				continue
			}
			if len(field.Names) > 0 {
				for _, ident := range field.Names {
					if ident == nil {
						continue
					}
					if j > len(argTypes)-1 {
						log.Println(name, argTypes, field.Names, j)
						continue
					}
					cd, err := makeContentDescriptor(argTypes[j], field, argIdent{ident, fmt.Sprintf("%sParameter%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Params = append(m.Params, &cd)
				}
			} else {
				cd, err := makeContentDescriptor(argTypes[j], field, argIdent{nil, fmt.Sprintf("%sParameter%d", name, j)})
				if err != nil {
					return m, err
				}
				j++
				m.Params = append(m.Params, &cd)
			}

		}
	}
	retTypes := documentGetRetTypes(cb)
	if fn.Type.Results != nil {
		j := 0
		for _, field := range fn.Type.Results.List {
			if field == nil {
				continue
			}
			if strings.Contains(fmt.Sprintf("%s", field.Type), "error") {
				continue
			}
			if len(field.Names) > 0 {
				// This really should never ever happen I don't think.
				// JSON-RPC returns _an_ result. So there can't be > 1 return value.
				// But just in case.
				for _, ident := range field.Names {
					cd, err := makeContentDescriptor(retTypes[j], field, argIdent{ident, fmt.Sprintf("%sResult%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Result = &cd
				}
			} else {
				cd, err := makeContentDescriptor(retTypes[j], field, argIdent{nil, fmt.Sprintf("%sResult", name)})
				if err != nil {
					return m, err
				}
				j++
				m.Result = &cd
			}
		}
	}

	return m, nil
}

/*
---
*/

//func (s *RPCService) Describe() (*goopenrpcT.OpenRPCSpec1, error) {
//
//	if s.doc == nil {
//		s.doc = NewOpenRPCDescription(s.server)
//	}
//
//	spl := strings.Split(s.doc.Doc.Info.Version, "+")
//	s.doc.Doc.Info.Version = fmt.Sprintf("%s-%s-%d", spl[0], time.Now().Format(time.RFC3339), time.Now().Unix())
//
//	for module, list := range s.methods() {
//		if module == "rpc" {
//			continue
//		}
//
//	methodListLoop:
//		for _, methodName := range list {
//			fullName := strings.Join([]string{module, methodName}, serviceMethodSeparators[0])
//			method := s.server.services.services[module].callbacks[methodName]
//
//			// FIXME: Development only.
//			// There is a bug with the isPubSub method, it's not picking up #PublicEthAPI.eth_subscribeSyncStatus
//			// because the isPubSub conditionals are wrong or the method is wrong.
//			if method.isSubscribe || strings.Contains(fullName, subscribeMethodSuffix) {
//				continue
//			}
//
//			// Dedupe. Not sure how `admin_datadir` got in there twice.
//			for _, m := range s.doc.Doc.Methods {
//				if m.Name == fullName {
//					continue methodListLoop
//				}
//			}
//			if err := s.doc.RegisterMethod(fullName, method); err != nil {
//				return nil, err
//			}
//		}
//	}
//
//	if err := Clean(s.doc.Doc); err != nil {
//		panic(err.Error())
//	}
//
//	return s.doc.Doc, nil
//}

// ---

type OpenRPCDescription struct {
	Doc *goopenrpcT.OpenRPCSpec1
}

func NewOpenRPCDescription(server *rpc.Server) *OpenRPCDescription {
	doc := &goopenrpcT.OpenRPCSpec1{
		OpenRPC: "1.2.4",
		Info: goopenrpcT.Info{
			Title:          "Ethereum JSON-RPC",
			Description:    "This API lets you interact with an EVM-based client via JSON-RPC",
			TermsOfService: "https://github.com/etclabscore/core-geth/blob/master/COPYING",
			Contact: goopenrpcT.Contact{
				Name:  "",
				URL:   "",
				Email: "",
			},
			License: goopenrpcT.License{
				Name: "Apache-2.0",
				URL:  "https://www.apache.org/licenses/LICENSE-2.0.html",
			},
			Version: "1.0.10",
		},
		Servers: []goopenrpcT.Server{},
		Methods: []goopenrpcT.Method{},
		Components: goopenrpcT.Components{
			ContentDescriptors:    make(map[string]*goopenrpcT.ContentDescriptor),
			Schemas:               make(map[string]spec.Schema),
			Examples:              make(map[string]goopenrpcT.Example),
			Links:                 make(map[string]goopenrpcT.Link),
			Errors:                make(map[string]goopenrpcT.Error),
			ExamplePairingObjects: make(map[string]goopenrpcT.ExamplePairing),
			Tags:                  make(map[string]goopenrpcT.Tag),
		},
		ExternalDocs: goopenrpcT.ExternalDocs{
			Description: "Source",
			URL:         "https://github.com/etclabscore/core-geth",
		},
		Objects: goopenrpcT.NewObjectMap(),
	}

	return &OpenRPCDescription{Doc: doc}
}

// Clean makes the openrpc validator happy.
// FIXME: Name me something better/organize me better.
func Clean(doc *goopenrpcT.OpenRPCSpec1) error {
	a := go_jsonschema_traverse.NewAnalysisT()

	//uniqueKeyFn := func(sch spec.Schema) string {
	//	b, _ := json.Marshal(sch)
	//	sum := sha1.Sum(b)
	//	out := fmt.Sprintf("%x", sum[:4])
	//
	//	if sch.Title != "" {
	//		out = fmt.Sprintf("%s.", sch.Title) + out
	//	}
	//
	//	if len(sch.Type) != 0 {
	//		out = fmt.Sprintf("%s.", strings.Join(sch.Type, "+")) + out
	//	}
	//
	//	spl := strings.Split(sch.Description, ":")
	//	splv := spl[len(spl)-1]
	//	if splv != "" && !strings.Contains(splv, " ") {
	//		out = splv + "_" + out
	//	}
	//
	//	return out
	//}

	doc.Components.Schemas = make(map[string]spec.Schema)
	for im := 0; im < len(doc.Methods); im++ {

		met := doc.Methods[im]
		//fmt.Println(met.Name)

		expander := func(sch *spec.Schema) error {
			return spec.ExpandSchema(sch, sch, nil)
		}

		/*
			removeDefinitions is a workaround to get rid of definitions at each schema,
			instead of doing what we probably should which is updating the reference uri against
			the document root
		*/
		removeDefinitions := func(sch *spec.Schema) error {
			sch.Definitions = nil
			return nil
		}

		//referencer := func(sch *spec.Schema) error {
		//
		//	schFingerprint := uniqueKeyFn(*sch)
		//	doc.Components.Schemas[schFingerprint] = *sch
		//	*sch = spec.Schema{
		//		SchemaProps: spec.SchemaProps{
		//			Ref: spec.Ref{
		//				Ref: jsonreference.MustCreateRef("#/components/schemas/" + schFingerprint),
		//			},
		//		},
		//	}
		//
		//	return nil
		//}

		// Params.
		for ip := 0; ip < len(met.Params); ip++ {
			par := met.Params[ip]
			//fmt.Println(" < ", par.Name)

			a.WalkDepthFirst(&par.Schema, expander)
			a.WalkDepthFirst(&par.Schema, removeDefinitions)
			//a.WalkDepthFirst(&par.Schema, referencer)
			met.Params[ip] = par
		}

		// Result (single).
		a.WalkDepthFirst(&met.Result.Schema, expander)
		a.WalkDepthFirst(&met.Result.Schema, removeDefinitions)
		//a.WalkDepthFirst(&met.Result.Schema, referencer)
	}

	return nil
}

func (d *OpenRPCDescription) RegisterMethod(name string, cb *rpc.callback) error {

	cb.makeArgTypes()
	cb.makeRetTypes()

	rtFunc := runtime.FuncForPC(cb.fn.Pointer())
	cbFile, _ := rtFunc.FileLine(rtFunc.Entry())

	tokenFileSet := token.NewFileSet()
	astFile, err := parser.ParseFile(tokenFileSet, cbFile, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	astFuncDecl := getAstFunc(cb, astFile, rtFunc)

	if astFuncDecl == nil {
		return fmt.Errorf("nil ast func: method name: %s", name)
	}

	method, err := makeMethod(name, cb, rtFunc, astFuncDecl)
	if err != nil {
		return fmt.Errorf("make method error method=%s cb=%s error=%v", name, spew.Sdump(cb), err)
	}

	d.Doc.Methods = append(d.Doc.Methods, method)
	sort.Slice(d.Doc.Methods, func(i, j int) bool {
		return d.Doc.Methods[i].Name < d.Doc.Methods[j].Name
	})

	return nil
}

type argIdent struct {
	ident *ast.Ident
	name  string
}

func (a argIdent) Name() string {
	if a.ident != nil {
		return a.ident.Name
	}
	return a.name
}

func makeMethod(name string, cb *rpc.callback, rt *runtime.Func, fn *ast.FuncDecl) (goopenrpcT.Method, error) {
	file, line := rt.FileLine(rt.Entry())

	//rt.Name()

	//packageName := strings.Split(rt.Name(), ".")[0]
	//
	//start, stop := fn.Pos(), fn.End()
	////fn.Body.List
	//

	ff := token.NewFileSet().AddFile(file, -1, 1024*1024*16)
	if ff == nil {
		panic("openrpc-method-tokenfileset-nilfile")
	}

	endline := ff.Line(fn.Body.End())

	fileOs, err := os.Open(file)
	if err != nil {
		return goopenrpcT.Method{}, err
	}
	b := []byte{}
	buf := bytes.NewBuffer(b)
	scanner := bufio.NewScanner(fileOs)
	iter := -1
	for scanner.Scan() {
		iter++
		if iter < line || iter > endline {
			continue
		}
		buf.Write(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		return goopenrpcT.Method{}, err
		//panic("openrpc-method-scanner-err")
	}
	fileOs.Close()

	m := goopenrpcT.Method{
		Name:        name,
		Tags:        []goopenrpcT.Tag{},
		Summary:     fn.Doc.Text(),
		Description: fmt.Sprintf("```\n%s\n```", string(buf.Bytes())), // rt.Name(),
		//  fmt.Sprintf("`%s`\n> [%s:%d][file://%s]", rt.Name(), file, line, file),
		//Description: "some words",
		ExternalDocs: goopenrpcT.ExternalDocs{
			Description: rt.Name(),
			URL:         fmt.Sprintf("file://%s:%d", file, line),
		},
		Params:         []*goopenrpcT.ContentDescriptor{},
		Result:         &goopenrpcT.ContentDescriptor{},
		Deprecated:     false,
		Servers:        []goopenrpcT.Server{},
		Errors:         []goopenrpcT.Error{},
		Links:          []goopenrpcT.Link{},
		ParamStructure: "by-position",
		Examples:       []goopenrpcT.ExamplePairing{},
	}

	defer func() {
		if m.Result.Name == "" {
			m.Result.Name = "null"
			m.Result.Schema.Type = []string{"null"}
			m.Result.Schema.Description = "Null"
		}
	}()

	if fn.Type.Params != nil {
		j := 0
		for _, field := range fn.Type.Params.List {
			if field == nil {
				continue
			}
			if cb.hasCtx && strings.Contains(fmt.Sprintf("%s", field.Type), "context") {
				continue
			}
			if len(field.Names) > 0 {
				for _, ident := range field.Names {
					if ident == nil {
						continue
					}
					if j > len(cb.argTypes)-1 {
						log.Println(name, cb.argTypes, field.Names, j)
						continue
					}
					cd, err := makeContentDescriptor(cb.argTypes[j], field, argIdent{ident, fmt.Sprintf("%sParameter%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Params = append(m.Params, &cd)
				}
			} else {
				cd, err := makeContentDescriptor(cb.argTypes[j], field, argIdent{nil, fmt.Sprintf("%sParameter%d", name, j)})
				if err != nil {
					return m, err
				}
				j++
				m.Params = append(m.Params, &cd)
			}

		}
	}
	if fn.Type.Results != nil {
		j := 0
		for _, field := range fn.Type.Results.List {
			if field == nil {
				continue
			}
			if strings.Contains(fmt.Sprintf("%s", field.Type), "error") {
				continue
			}
			if len(field.Names) > 0 {
				// This really should never ever happen I don't think.
				// JSON-RPC returns _an_ result. So there can't be > 1 return value.
				// But just in case.
				for _, ident := range field.Names {
					cd, err := makeContentDescriptor(cb.retTypes[j], field, argIdent{ident, fmt.Sprintf("%sResult%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Result = &cd
				}
			} else {
				cd, err := makeContentDescriptor(cb.retTypes[j], field, argIdent{nil, fmt.Sprintf("%sResult", name)})
				if err != nil {
					return m, err
				}
				j++
				m.Result = &cd
			}
		}
	}

	return m, nil
}

func makeContentDescriptor(ty reflect.Type, field *ast.Field, ident argIdent) (goopenrpcT.ContentDescriptor, error) {
	cd := goopenrpcT.ContentDescriptor{}
	if !jsonschemaPkgSupport(ty) {
		return cd, fmt.Errorf("unsupported iface: %v %v %v", spew.Sdump(ty), spew.Sdump(field), spew.Sdump(ident))
	}

	schemaType := fmt.Sprintf("%s:%s", ty.PkgPath(), ty.Name())
	switch tt := field.Type.(type) {
	case *ast.SelectorExpr:
		schemaType = fmt.Sprintf("%v.%v", tt.X, tt.Sel)
		schemaType = fmt.Sprintf("%s:%s", ty.PkgPath(), schemaType)
	case *ast.StarExpr:
		schemaType = fmt.Sprintf("%v", tt.X)
		schemaType = fmt.Sprintf("*%s:%s", ty.PkgPath(), schemaType)
		if reflect.ValueOf(ty).Type().Kind() == reflect.Ptr {
			schemaType = fmt.Sprintf("%v", ty.Elem().Name())
			schemaType = fmt.Sprintf("*%s:%s", ty.Elem().PkgPath(), schemaType)
		}
		//ty = ty.Elem() // FIXME: wart warn
	}
	//schemaType = fmt.Sprintf("%s:%s", ty.PkgPath(), schemaType)

	cd.Name = ident.Name()

	cd.Summary = field.Comment.Text()              // field.Doc.Text()
	cd.Description = fmt.Sprintf("%s", schemaType) // field.Comment.Text()

	rflctr := jsonschema.Reflector{
		AllowAdditionalProperties:  true, // false,
		RequiredFromJSONSchemaTags: true,
		ExpandedStruct:             false, // false, // false,
		//IgnoredTypes:               []interface{}{chaninterface},
		TypeMapper: OpenRPCJSONSchemaTypeMapper,
	}

	jsch := rflctr.ReflectFromType(ty)

	// Poor man's type cast.
	// Need to get the type from the go struct -> json reflector package
	// to the swagger/go-openapi/jsonschema spec.
	// Do this with JSON marshaling.
	// Hacky? Maybe. Effective? Maybe.
	m, err := json.Marshal(jsch)
	if err != nil {
		log.Fatal(err)
	}
	sch := spec.Schema{}
	err = json.Unmarshal(m, &sch)
	if err != nil {
		log.Fatal(err)
	}
	// End Hacky maybe.
	if schemaType != ":" && (cd.Schema.Description == "" || cd.Schema.Description == ":") {
		sch.Description = schemaType
	}

	cd.Schema = sch

	return cd, nil
}

func jsonschemaPkgSupport(r reflect.Type) bool {
	switch r.Kind() {
	case reflect.Struct,
		reflect.Map,
		reflect.Slice, reflect.Array,
		reflect.Interface,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Bool,
		reflect.String,
		reflect.Ptr:
		return true
	default:
		return false
	}
}

type schemaDictEntry struct {
	t interface{}
	j string
}

func OpenRPCJSONSchemaTypeMapper(r reflect.Type) *jsonschema.Type {
	unmarshalJSONToJSONSchemaType := func(input string) *jsonschema.Type {
		var js jsonschema.Type
		err := json.Unmarshal([]byte(input), &js)
		if err != nil {
			panic(err)
		}
		return &js
	}

	integerD := `{
          "title": "integer",
          "type": "string",
          "pattern": "^0x[a-fA-F0-9]+$",
          "description": "Hex representation of the integer"
        }`
	commonHashD := `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`
	blockNumberTagD := `{
	     "title": "blockNumberTag",
	     "type": "string",
	     "description": "The optional block height description",
	     "enum": [
	       "earliest",
	       "latest",
	       "pending"
	     ]
	   }`

	blockNumberOrHashD := fmt.Sprintf(`{
          "oneOf": [
            %s,
            %s
          ]
        }`, blockNumberTagD, commonHashD)

	//s := jsonschema.Reflect(ethapi.Progress{})
	//ethSyncingResultProgress, err := json.Marshal(s)
	//if err != nil {
	//	return nil
	//}

	// Second, handle other types.
	// Use a slice instead of a map because it preserves order, as a logic safeguard/fallback.
	dict := []schemaDictEntry{

		{new(big.Int), integerD},
		{big.Int{}, integerD},
		{new(hexutil.Big), integerD},
		{hexutil.Big{}, integerD},

		{types.BlockNonce{}, integerD},

		{new(common.Address), `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash POINTER",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`},

		{common.Address{}, `{
          "title": "address",
          "type": "string",
          "pattern": "^0x[a-fA-F\\d]{40}$"
        }`},

		{new(common.Hash), `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash POINTER",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`},

		{common.Hash{}, commonHashD},

		{
			hexutil.Bytes{}, `{
          "title": "dataWord",
          "type": "string",
          "description": "Hex representation of a 256 bit unit of data",
          "pattern": "^0x([a-fA-F\\d]{64})?$"
        }`},
		{
			new(hexutil.Bytes), `{
          "title": "dataWord",
          "type": "string",
          "description": "Hex representation of a 256 bit unit of data",
          "pattern": "^0x([a-fA-F\\d]{64})?$"
        }`},

		{[]byte{}, `{
          "title": "bytes",
          "type": "string",
          "description": "Hex representation of a variable length byte array",
          "pattern": "^0x([a-fA-F0-9]?)+$"
        }`},

		{rpc.BlockNumber(0),
			blockNumberOrHashD,
		},

		{rpc.BlockNumberOrHash{}, fmt.Sprintf(`{
		  "title": "blockNumberOrHash",
		  "oneOf": [
			%s,
			{
			  "allOf": [
				%s,
				{
				  "type": "object",
				  "properties": {
					"requireCanonical": {
					  "type": "boolean"
					}
				  },
				  "additionalProperties": false
				}
			  ]
			}
		  ]
		}`, blockNumberOrHashD, blockNumberOrHashD)},

		//{
		//	BlockNumber(0): blockNumberOrHashD,
		//},

		//{BlockNumberOrHash{}, fmt.Sprintf(`{
		//  "title": "blockNumberOrHash",
		//  "description": "Hex representation of a block number or hash",
		//  "oneOf": [%s, %s]
		//}`, commonHashD, integerD)},

		//{BlockNumber(0), fmt.Sprintf(`{
		//  "title": "blockNumberOrTag",
		//  "description": "Block tag or hex representation of a block number",
		//  "oneOf": [%s, %s]
		//}`, commonHashD, blockNumberTagD)},

		//		{ethapi.EthSyncingResult{}, fmt.Sprintf(`{
		//          "title": "ethSyncingResult",
		//		  "description": "Syncing returns false in case the node is currently not syncing with the network. It can be up to date or has not
		//yet received the latest block headers from its pears. In case it is synchronizing:
		//- startingBlock: block number this node started to synchronise from
		//- currentBlock:  block number this node is currently importing
		//- highestBlock:  block number of the highest block header this node has received from peers
		//- pulledStates:  number of state entries processed until now
		//- knownStates:   number of known state entries that still need to be pulled",
		//		  "oneOf": [%s, %s]
		//		}`, `{
		//        "type": "boolean"
		//      }`, `{"type": "object"}`)},

	}

	for _, d := range dict {
		d := d
		if reflect.TypeOf(d.t) == r {
			tt := unmarshalJSONToJSONSchemaType(d.j)

			return tt
		}
	}

	// First, handle primitives.
	switch r.Kind() {
	case reflect.Struct:

	case reflect.Map,
		reflect.Interface:
	case reflect.Slice, reflect.Array:

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		ret := unmarshalJSONToJSONSchemaType(integerD)
		return ret

	case reflect.Float32, reflect.Float64:

	case reflect.Bool:

	case reflect.String:

	case reflect.Ptr:

	default:
		panic("prevent me somewhere else please")
	}

	return nil
}

func getAstFunc(cb *rpc.callback, astFile *ast.File, rf *runtime.Func) *ast.FuncDecl {

	rfName := runtimeFuncName(rf)
	for _, decl := range astFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name == nil || fn.Name.Name != rfName {
			continue
		}
		//log.Println("getAstFunc", spew.Sdump(cb), spew.Sdump(fn))
		fnRecName := ""
		for _, l := range fn.Recv.List {
			if fnRecName != "" {
				break
			}
			i, ok := l.Type.(*ast.Ident)
			if ok {
				fnRecName = i.Name
				continue
			}
			s, ok := l.Type.(*ast.StarExpr)
			if ok {
				fnRecName = fmt.Sprintf("%v", s.X)
			}
		}
		// Ensure that this is the one true function.
		// Have to match receiver AND method names.
		/*
		 => recvr= <*ethapi.PublicBlockChainAPI Value> fn= PublicBlockChainAPI
		 => recvr= <*ethash.API Value> fn= API
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*node.PrivateAdminAPI Value> fn= PrivateAdminAPI
		 => recvr= <*node.PublicAdminAPI Value> fn= PublicAdminAPI
		 => recvr= <*node.PublicAdminAPI Value> fn= PublicAdminAPI
		 => recvr= <*eth.PrivateAdminAPI Value> fn= PrivateAdminAPI
		*/

		reRec := regexp.MustCompile(fnRecName + `\s`)
		if !reRec.MatchString(cb.rcvr.String()) {
			continue
		}
		return fn
	}
	return nil
}

//func getAstType(astFile *ast.File, t reflect.Type) *ast.TypeSpec {
//	log.Println("getAstType", t.Name(), t.String())
//	for _, decl := range astFile.Decls {
//		d, ok := decl.(*ast.GenDecl)
//		if !ok {
//			continue
//		}
//		if d.Tok != token.TYPE {
//			continue
//		}
//		for _, s := range d.Specs {
//			sp, ok := s.(*ast.TypeSpec)
//			if !ok {
//				continue
//			}
//			if sp.Name != nil && sp.Name.Name == t.Name() {
//				return sp
//			} else if sp.Name != nil {
//				log.Println("nomatch", sp.Name.Name)
//			}
//		}
//
//	}
//	return nil
//}

func runtimeFuncName(rf *runtime.Func) string {
	spl := strings.Split(rf.Name(), ".")
	return spl[len(spl)-1]
}

func (d *OpenRPCDescription) findMethodByName(name string) (ok bool, method goopenrpcT.Method) {
	for _, m := range d.Doc.Methods {
		if m.Name == name {
			return true, m
		}
	}
	return false, goopenrpcT.Method{}
}

//func runtimeFuncPackageName(rf *runtime.Func) string {
//	re := regexp.MustCompile(`(?im)^(?P<pkgdir>.*/)(?P<pkgbase>[a-zA-Z0-9\-_]*)`)
//	match := re.FindStringSubmatch(rf.Name())
//	pmap := make(map[string]string)
//	for i, name := range re.SubexpNames() {
//		if i > 0 && i <= len(match) {
//			pmap[name] = match[i]
//		}
//	}
//	return pmap["pkgdir"] + pmap["pkgbase"]
//}