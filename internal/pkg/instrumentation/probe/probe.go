// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package probe provides instrumentation probe types and definitions.
package probe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/go-logr/logr"

	"go.opentelemetry.io/auto/internal/pkg/inject"
	"go.opentelemetry.io/auto/internal/pkg/instrumentation/bpffs"
	"go.opentelemetry.io/auto/internal/pkg/instrumentation/utils"
	"go.opentelemetry.io/auto/internal/pkg/process"
	"go.opentelemetry.io/auto/internal/pkg/structfield"
)

// Probe is the instrument used by instrumentation for a Go package to measure
// and report on the state of that packages operation.
type Probe interface {
	// LibraryName returns the package name being instrumented.
	LibraryName() string

	// FuncNames returns the fully-qualified function names that are
	// instrumented.
	FuncNames() []string

	// Load loads all instrumentation offsets.
	Load(*link.Executable, *process.TargetDetails) error

	// Run runs the events processing loop.
	Run(eventsChan chan<- *Event)

	// Close stops the Probe.
	Close()
}

// Base is a base implementation of [Probe].
//
// This type can be returned by instrumentation directly. Instrumentation can
// also wrap this implementation with their own type if they need to override
// default behavior.
type Base[BPFObj any, BPFEvent any] struct {
	// Name is the unique name of the instrumentation probe.
	Name string
	// Logger is used to log operations and errors.
	Logger logr.Logger

	// InstrumentedPkg is the package path of the instrumented code.
	InstrumentedPkg string
	// Consts are the constants that need to be injected into the eBPF program
	// that is run by this Probe.
	Consts []Const
	// Uprobes is a mapping from runtime symbols to a UprobeFunc. These define
	// the eBPF program triggers that need to setup for this Probe.
	Uprobes map[string]UprobeFunc[BPFObj]

	// ReaderFn is a creation function for a perf.Reader based on the passed
	// BPFObj related to the probe.
	ReaderFn func(BPFObj) (*perf.Reader, error)
	// SpecFn is a creation function for an eBPF CollectionSpec related to the
	// probe.
	SpecFn func() (*ebpf.CollectionSpec, error)
	// ProcessFn processes probe events into a uniform Event type.
	ProcessFn func(*BPFEvent) *Event

	reader  *perf.Reader
	closers []io.Closer
}

// LibraryName returns the package name being instrumented.
func (i *Base[BPFObj, BPFEvent]) LibraryName() string {
	return i.InstrumentedPkg
}

// FuncNames returns the fully-qualified function names that are instrumented.
func (i *Base[BPFObj, BPFEvent]) FuncNames() []string {
	symbols := make([]string, 0, len(i.Uprobes))
	for s := range i.Uprobes {
		symbols = append(symbols, s)
	}

	sort.Strings(symbols)

	return symbols
}

// Load loads all instrumentation offsets.
func (i *Base[BPFObj, BPFEvent]) Load(exec *link.Executable, td *process.TargetDetails) error {
	spec, err := i.SpecFn()
	if err != nil {
		return err
	}

	err = i.injectConsts(td, spec)
	if err != nil {
		return err
	}

	obj, err := i.buildObj(exec, td, spec)
	if err != nil {
		return err
	}

	i.reader, err = i.ReaderFn(*obj)
	if err != nil {
		return err
	}
	i.closers = append(i.closers, i.reader)

	return nil
}

func (i *Base[BPFObj, BPFEvent]) injectConsts(td *process.TargetDetails, spec *ebpf.CollectionSpec) error {
	opts, err := consts(i.Consts).injectOpts(td)
	if err != nil {
		return err
	}
	return inject.Constants(spec, opts...)
}

func (i *Base[BPFObj, BPFEvent]) buildObj(exec *link.Executable, td *process.TargetDetails, spec *ebpf.CollectionSpec) (*BPFObj, error) {
	obj := new(BPFObj)
	if c, ok := ((interface{})(obj)).(io.Closer); ok {
		i.closers = append(i.closers, c)
	}

	sOpts := &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: bpffs.PathForTargetApplication(td),
		},
	}
	err := utils.LoadEBPFObjects(spec, obj, sOpts)
	if err != nil {
		return nil, err
	}

	for symb, f := range i.Uprobes {
		links, err := f(symb, exec, td, obj)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			i.closers = append(i.closers, l)
		}
	}

	return obj, nil
}

// Run runs the events processing loop.
func (i *Base[BPFObj, BPFEvent]) Run(dest chan<- *Event) {
	for {
		record, err := i.reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			i.Logger.Error(err, "error reading from perf reader")
			continue
		}

		if record.LostSamples != 0 {
			i.Logger.Info("perf event ring buffer full", "dropped", record.LostSamples)
			continue
		}

		e, err := i.processRecord(record)
		if err != nil {
			i.Logger.Error(err, "failed to process perf record")
		}

		dest <- e
	}
}

func (i *Base[BPFObj, BPFEvent]) processRecord(record perf.Record) (*Event, error) {
	buf := bytes.NewBuffer(record.RawSample)

	var event BPFEvent
	err := binary.Read(buf, binary.LittleEndian, &event)
	if err != nil {
		return nil, err
	}
	return i.ProcessFn(&event), nil
}

// Close stops the Probe.
func (i *Base[BPFObj, BPFEvent]) Close() {
	var err error
	for _, c := range i.closers {
		err = errors.Join(err, c.Close())
	}
	if err != nil {
		i.Logger.Error(err, "failed to cleanup", "Probe", i.Name)
	}
}

// UprobeFunc is a function that will attach a eBPF program to a perf event
// that fires when the given symbol starts executing in exec.
//
// It is expected the symbol belongs to are shared library and its offset can
// be determined using target.
//
// Losing the reference to the resulting Link (up) will close the Uprobe and
// prevent further execution of prog. The Link must be Closed during program
// shutdown to avoid leaking system resources.
type UprobeFunc[BPFObj any] func(symbol string, exec *link.Executable, target *process.TargetDetails, obj *BPFObj) ([]link.Link, error)

// Const is an constant that needs to be injected into an eBPF program.
type Const interface {
	// InjectOption returns the inject.Option to run for the Const when running
	// inject.Constants.
	InjectOption(td *process.TargetDetails) (inject.Option, error)
}

type consts []Const

func (c consts) injectOpts(td *process.TargetDetails) ([]inject.Option, error) {
	var (
		out []inject.Option
		err error
	)
	for _, cnst := range c {
		o, e := cnst.InjectOption(td)
		err = errors.Join(err, e)
		if e == nil {
			out = append(out, o)
		}
	}
	return out, err
}

// StructFieldConst is a [Const] for a struct field offset. These struct field
// ID needs to be known offsets in the [inject] package.
type StructFieldConst struct {
	Key string
	Val structfield.ID
}

// InjectOption returns the appropriately configured [inject.WithOffset] if the
// version of the struct field module is known. If it is not, an error is
// returned.
func (c StructFieldConst) InjectOption(td *process.TargetDetails) (inject.Option, error) {
	ver, ok := td.Libraries[c.Val.ModPath]
	if !ok {
		return nil, fmt.Errorf("unknown module version: %s", c.Val.ModPath)
	}
	return inject.WithOffset(c.Key, c.Val, ver), nil
}

// AllocationConst is a [Const] for all the allocation details that need to be
// injected into an eBPF program.
type AllocationConst struct{}

// InjectOption returns the appropriately configured
// [inject.WithAllocationDetails] if the [process.AllocationDetails] within td
// are not nil. An error is returned if [process.AllocationDetails] is nil.
func (c AllocationConst) InjectOption(td *process.TargetDetails) (inject.Option, error) {
	if td.AllocationDetails == nil {
		return nil, errors.New("no allocation details")
	}
	return inject.WithAllocationDetails(*td.AllocationDetails), nil
}

// RegistersABIConst is a [Const] for the boolean flag informing an eBPF
// program if the Go space has registered ABI.
type RegistersABIConst struct{}

// InjectOption returns the appropriately configured [inject.WithRegistersABI].
func (c RegistersABIConst) InjectOption(td *process.TargetDetails) (inject.Option, error) {
	return inject.WithRegistersABI(td.IsRegistersABI()), nil
}

// KeyValConst is a [Const] for a generic key-value pair.
//
// This should not be used as a replacement for any of the other provided
// [Const] implementations. Those implementations may have added significance
// and should be used instead where applicable.
type KeyValConst struct {
	Key string
	Val interface{}
}

// InjectOption returns the appropriately configured [inject.WithKeyValue].
func (c KeyValConst) InjectOption(*process.TargetDetails) (inject.Option, error) {
	return inject.WithKeyValue(c.Key, c.Val), nil
}
