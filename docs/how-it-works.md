# OpenTelemtry Go Instrumentation - How it works

We aim to bring the automatic instrumentation experience found in languages like [Java](https://github.com/open-telemetry/opentelemetry-java-instrumentation), [Python](https://github.com/open-telemetry/opentelemetry-python-contrib) and [JavaScript](https://github.com/open-telemetry/opentelemetry-js-contrib) to Go applications.

## Design Goals

- No code changes required - any Go application can be instrumented without modifying the source code.
- Support wide range of Go applications - instrumentation is supported for Go version 1.12 and above. In addition, a common practice for Go applications is to shrink the binary size by stripping debug symbols via `go build -ldflags "-s -w"`. This instrumentation works for stripped binaries as well.
- Configuration is done via `OTEL_*` environment variables according to [OpenTelemetry Environment Variable Specification](https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/sdk-environment-variables.md#general-sdk-configuration)
- Instrumented libraries follow the [OpenTelemetry specification](https://github.com/open-telemetry/opentelemetry-specification) and semantic conventions to produce standard OpenTelemetry data.

## Why eBPF

Go is a compiled language. Unlike languages such as Java and Python, Go compiles natively to machine code. This makes it impossible to add additional code at runtime to instrument Go applications.
Fortunately, the Linux kernel provides a mechanism to attach user-defined code to the execution of a process. This is called [eBPF](https://ebpf.io/) and it is widely used in other Cloud Native projects such as Cilium and Falco.

## Main Challenges and How We Overcome Them

Using eBPF to instrument Go applications is non-trivial. In the following sections we will describe the main challenges and how we solved them.

### Instrumentation Stability

eBPF programs access user code and variables by analyzing the stack and the CPU registers. For example, to read the value of the `target` field in the `google.golang.org/grpc.ClientConn` struct (see gRPC instrumentor for an example), the eBPF program needs to know the offset of the field inside the struct. The offset is determined by the field location inside the struct definition.

Hard coding this offset information into the eBPF programs creates a very unstable instrumentation. Fields locations inside structs are subject to change and the eBPF program needs to be recompiled every time the struct definition changes.
Luckily for us, there is a way to analyze the target binary and extract the required offsets, by using DWARF. The DWARF debug information is generated by the compiler and is stored inside the binary.

Notice that one of our design goals is to support stripped Go binaries - meaning binaries that do not contain debug information. In order to support stripped binaries and to create a stable instrumentation, we created a library called [offsets-tracker](https://github.com/keyval-dev/offsets-tracker). This library tracks the offset of different fields across versions.

We currently track instrumented structs inside the Go standard library and selected open source packages. This solution does not require DWARF information on the target binary and provides stability to instrumentations. Instrumentation authors can get a field location by name instead of hard coding a field offset.

The offsets-tracker generates the [offset_results.json](https://github.com/keyval-dev/opentelemetry-go-instrumentation/blob/master/pkg/inject/offset_results.json) file. This file contains the offsets of the fields in the instrumented structs.

### Uretprobes

One of the basic requirments of OpenTelemetry spans is to contain start timestamp and end timestamp. Getting those timestamps is possible by placing an eBPF code at the start and the end of the instrumented function. eBPF supports this requirement via uprobes and uretprobes. Uretprobes are used to invoke eBPF code at the end of the function. Unfortunately, uretprobes and Go [do not play well together](https://github.com/golang/go/issues/22008).

We overcome this issue by analyzing the target binary and detecting all the return statements in the instrumented functions. We then place a uprobe at the end of each return statement. This uprobe invokes the eBPF code that collects the end timestamp.

### Timestamp tracking

eBPF programs can access the current timestamp by calling `bpf_ktime_get_ns()`. The value returned by this function is fetched from the `CLOCK_MONOTONIC` clock and represents the number of nanoseconds since the system boot time.

According to OpenTelemetry specification start time and end time should be timestamps and represent exact point in time. Converting from monotonic time to epoch timestamp is automatically handled by this library. Conversion is achieved by discovering the epoch boot time and adding it to the monotonic time collected by the eBPF program.

### Support Go 1.17 and above

Since version 1.17 and above, Go [changed the way it passes arguments to functions](https://go.googlesource.com/go/+/refs/heads/dev.regabi/src/cmd/compile/internal-abi.md#function-call-argument-and-result-passing).
Prior to version 1.17, Go placed arguments in the stack in the order they were defined in the function signature. Version 1.17 and above uses the machine registers to pass arguments.

We overcome this by analyzing the target binary and detecting the compiled Go version. If the compiled Go version is 1.17 or above, we read arguments from the machine registers. If the compiled Go version is below 1.17, we read arguments from the stack. This should be transparent to the instrumentation authors and abstracted by a function named `get_argument()`.