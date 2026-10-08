# Comprehensive Go

> A zero-to-advanced tour of Go, grounded in this codebase. Whether you're
> new to Go or coming back after a while, this is designed to work two
> ways: read it linearly to build a mental model, or grep into it later
> when you forget a specific detail.

## How to read this

- **Linear read** of Parts 1-3 is enough to read and write everyday Go.
  In roughly the time it takes to fly Bangkok -> Singapore you can
  absorb the basics, the test style, and the concurrency model.
- **Parts 4 and 5** are reference material. Skim once, then return
  when you actually need generics, reflection, or pprof.
- Code samples use idiomatic modern Go (1.22+) and were written against
  the same compiler we use here (`go 1.27.1`). They reference files in
  this repo where useful — the goal is that what you learn maps 1:1 to
  what you will write tomorrow.
- This document is intentionally long because each topic is short. If
  you want a quickstart, jump to the Appendix A reading order.

## Conventions used in this document

- `[In this repo]` callouts link to files in this codebase where the
  concept is actually used.
- `[Gotcha]` flags a frequent stumbling block.
- `[Go deeper]` points at the canonical reference.
- Code samples are production-shaped (the kind of Go you'd actually
  write), not toy puzzles.

---

## Table of contents

- **Part 1 - The Basics**
  1. Hello, World
  2. Packages and modules
  3. Variables, types, constants
  4. Functions and multiple returns
  5. Control flow: if, for, switch
  6. Errors and error handling
  7. defer, panic, recover
  8. Arrays and slices
  9. Maps
  10. Structs and methods
  11. Pointers
  12. Strings, bytes, runes
  13. Printing and logging
  14. Comments and godoc

- **Part 2 - Building Blocks**
  15. Interfaces (small ones)
  16. Composition via embedding
  17. Type assertions and switches
  18. The empty interface and `any`
  19. JSON encoding
  20. Testing basics
  21. testify and table-driven tests

- **Part 3 - Concurrency**
  22. Goroutines
  23. Channels
  24. `select` statement
  25. `context.Context`
  26. `sync.WaitGroup`, `sync.Mutex`, `sync.Once`
  27. `errgroup`
  28. Worker pools, pipelines, fan-out/fan-in

- **Part 4 - Intermediate Tools**
  29. Generics (Go 1.18+)
  30. `iter` package (Go 1.23+)
  31. `//go:embed`
  32. Reflection (use sparingly)
  33. Build tags and OS/arch handling
  34. Modules, workspaces, dependency hygiene

- **Part 5 - Advanced & Practical Wisdom**
  35. The Go memory model
  36. Detecting data races with `-race`
  37. Goroutine and channel leaks
  38. Profiling with pprof
  39. Graceful shutdown (this repo's pattern)
  40. Common pitfalls

- **Appendix**
  A. Reading order for a quickstart
  B. Where to learn more

---

# Part 1 - The Basics

## 1. Hello, World

Every Go file starts with `package`. Files in a single directory belong
to the same package. There are two flavours:

- `package main` - produces an executable; the file MUST contain a
  `func main()`.
- `package foo` - produces a library; no `main()`, but it can have
  any number of `func`s.

```go
// File: hello.go
package main

import "fmt"

func main() {
    fmt.Println("hello, world")
}
```

Run it: `go run hello.go`. Build it: `go build hello.go`.

[Gotcha] Only `package main` produces an executable. Trying to `go run`
a `package foo` file gives `go run: no command-line arguments`.

[Go deeper] https://go.dev/doc/code

---

## 2. Packages and modules

A **package** is a directory of `.go` files. A **module** is a tree of
packages declared by `go.mod`. You build modules; you import packages.

```go
// go.mod
module github.com/emmitt-k/ticket-deal

go 1.27.1

require (
    github.com/go-chi/chi/v5 v5.3.2
    github.com/golang-jwt/jwt/v5 v5.3.1
)
```

Import paths reflect URLs so that any server can host code without
permission. Even if your code never moves off your laptop, this
import path is what every other file uses.

```go
import (
    "fmt"                                                    // standard library
    "github.com/redis/go-redis/v9"                           // third-party
    "github.com/emmitt-k/ticket-deal/internal/auth"          // your own
)
```

[In this repo] `go.mod` declares module
`github.com/emmitt-k/ticket-deal`. The `internal/` prefix is a Go
convention: packages under `internal/` cannot be imported from outside
the module, so you can refactor them freely without breaking callers.

[Gotcha] Don't use a Go vanity URL like `tickets-project/x` unless you
own `tickets-project`. If you do, use the rule `import path = URL
where the code actually lives`. For private code, a module path like
`mycompany.local/tickets` is perfectly acceptable.

[Gotcha] `internal/` is a magic directory name. Anything you put under
`internal/yourpkg` is importable only by code under your own module's
root.

[Go deeper] https://go.dev/doc/modules

---

## 3. Variables, types, constants

Go has a small, explicit type system. Most types are well-defined and
there is rarely surprise. The compiler is strict; that strictness is
a feature.

```go
package main

import "fmt"

const pi = 3.14159                  // untyped constant, very flexible
const name string = "ticket-deal"  // typed constant

var count int                      // zero value: 0
var s string = "hello"             // explicit type and value

b := true                          // short-declaration; type inferred
c := 1 + 2i                        // complex128

// Multiple assignment from a function:
n, err := fmt.Println("hi")        // n=3, err=nil
_, _ = n, err                      // discard both

// Multiple vars in one block:
var (
    debug   = false
    timeout = 30 * time.Second
)
```

Zero values matter - they are the safe defaults.

| Type                       | Zero value      |
| -------------------------- | --------------- |
| `int`, `float64`           | `0`             |
| `string`                   | `""`            |
| `bool`                     | `false`         |
| `*T` (pointer)             | `nil`           |
| `[]T` (slice)              | `nil`           |
| `map[K]V`                  | `nil`           |
| `chan T`                   | `nil`           |
| `func(...)...`             | `nil`           |
| struct                     | all fields zero |

This is why Go doesn't need constructors - you can declare a variable
and immediately use it, and `if err != nil` covers the rest.

[Gotcha] `:=` only works inside functions, and it requires at least one
new variable on the left side. In a long function it's sometimes easier
to start with `var x = ...` to keep type clarity.

[Go deeper] https://go.dev/ref/spec#The_zero_value

---

## 4. Functions and multiple returns

Functions can return multiple values. The most common pattern is
`(result, error)`.

```go
package main

import (
    "errors"
    "fmt"
    "strconv"
)

func parseInt(s string) (int, error) {
    n, err := strconv.Atoi(s)
    if err != nil {
        return 0, fmt.Errorf("parseInt(%q): %w", s, err)
    }
    return n, nil
}

// Named return values: documented at the signature, automatically
// returned if you write a bare `return`.
func divide(a, b float64) (quotient, remainder float64, err error) {
    if b == 0 {
        err = errors.New("divide by zero")
        return
    }
    quotient = a / b
    remainder = a - quotient*b
    return
}

func main() {
    n, err := parseInt("42")
    if err != nil {
        fmt.Println("oops:", err)
        return
    }
    fmt.Println(n)
}
```

Variadic parameters accept zero or more arguments of the listed type:

```go
func logf(format string, args ...any) {
    fmt.Printf(format+"\n", args...)
}

logf("hello %s", "world")
logf("nothing to interpolate")    // args is empty slice, fine
```

[In this repo] Almost every function in `internal/auth/jwt.go` returns
`(*Claims, error)`. Idiomatic Go - the success value is the first
return, the error is the last.

[Gotcha] Naked `return` is fine, but it makes you scan the function
twice. Most production Go either omits it entirely or hides it inside
small, well-named functions.

[Gotcha] `fmt.Errorf("...: %w", err)` wraps the original error so
`errors.Is` and `errors.As` can still find it. Without `%w` the chain
breaks - see Section 6.

[Go deeper] https://go.dev/ref/spec#Function_declarations

---

## 5. Control flow: if, for, switch

There is no `while` in Go. `for` covers all looping.

```go
package main

import "fmt"

func main() {
    // traditional for
    for i := 0; i < 3; i++ {
        fmt.Println(i)
    }

    // while-style
    n := 0
    for n < 3 {
        n++
    }

    // forever
    for {
        break
    }

    // range over slice, map, channel, string, integer
    xs := []string{"a", "b"}
    for i, x := range xs {
        fmt.Println(i, x)
    }
    for _, x := range xs {        // ignore index
        fmt.Println(x)
    }

    // if with a short statement; common for pre-checks
    if v, err := lookup(); err != nil {
        return err
    } else {
        use(v)
    }

    // switch with no expression (tagless)
    switch {
    case n%2 == 0:
        fmt.Println("even")
    default:
        fmt.Println("odd")
    }

    // type switch (Section 17)
    // switch x := y.(type) { case int: ...; case string: ... }
}
```

A subtle point: `for i, x := range xs` copies `xs` once before the
loop starts. That's normally what you want, but if `xs` is huge and
you intend to mutate it inside the loop, range won't see your changes.

[Go deeper] https://go.dev/ref/spec#Statements

---

## 6. Errors and error handling

`error` is an interface with one method:

```go
type error interface {
    Error() string
}
```

That's it. Anyone can satisfy it.

```go
package main

import (
    "errors"
    "fmt"
)

// A sentinel error that callers can match against.
var ErrNotFound = errors.New("not found")

// A custom error type that carries context.
type ValidationError struct {
    Field string
    Msg   string
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation: %s: %s", e.Field, e.Msg)
}

// Wrap and rewrap errors. The `%w` verb preserves the chain.
func loadUser(id int) (string, error) {
    return "", fmt.Errorf("loadUser(%d): %w", id, ErrNotFound)
}

func main() {
    _, err := loadUser(7)
    if errors.Is(err, ErrNotFound) {
        fmt.Println("not found:", err)
    }

    var verr *ValidationError
    if errors.As(loadUser(7), &verr) {
        fmt.Println("validation issue:", verr.Field)
    }
}
```

| Construct         | When                                                        |
| ----------------- | ----------------------------------------------------------- |
| `errors.New(...)` | Simple sentinel error                                       |
| `fmt.Errorf("...%w", err)` | Wrap while keeping the chain (`%w` is the key)   |
| `errors.Is(err, target)` | Match against a specific error in the chain         |
| `errors.As(err, &t)`      | Pull the most specific matching error into `t`      |

[In this repo] `internal/auth/jwt.go` defines sentinel errors
`ErrTokenExpired`, `ErrTokenSignature`, etc. and uses `errors.Is` in
both the library (`classifyError`) and the middleware
(`classifyAuthError`).

[Gotcha] Don't compare with `==` against an error from `errors.New`.
Use `errors.Is`. That way the chain works: a wrapped error
(`fmt.Errorf("%w", ErrTokenExpired)`) still matches the sentinel.

[Go deeper] https://go.dev/blog/go1.13-errors

---

## 7. defer, panic, recover

`defer` schedules a function call to run when the surrounding function
returns. Useful for cleanup.

```go
package main

import (
    "fmt"
    "os"
)

func main() {
    f, err := os.Open("data.txt")
    if err != nil {
        fmt.Println(err)
        return
    }
    defer f.Close()             // runs when main returns, even on panic

    // do stuff with f ...
}
```

`defer`s run in LIFO order (last deferred, first served). Arguments are
evaluated when `defer` is called, not when the function runs.

```go
func a() {
    for i := 0; i < 3; i++ {
        defer fmt.Println(i)        // 2, 1, 0
    }
}
```

### Panic and recover

`panic` raises a run-time error that unwinds the stack, running all
deferred functions on the way. `recover`, inside a deferred function,
catches the panic.

```go
func safeCall() {
    defer func() {
        if r := recover(); r != nil {
            fmt.Println("recovered:", r)
        }
    }()
    willPanic()
}

func willPanic() {
    panic("nope")
}
```

When to use `panic`:

- Truly unrecoverable conditions - you can't allocate memory, or your
  invariants are so broken that continuing is meaningless.
- Programmer errors - "this should never happen" bugs in init code.
- The standard library panics on out-of-range slice indexes.

In application code, prefer returning errors. Reserve panics for
"can't go on" cases.

[In this repo] `cmd/api/main.go` uses `log.Fatalf` (which is roughly
"panic + exit 1"). For an API server, fail-fast at startup is usually
the right behaviour - we cannot serve traffic with a broken
configuration.

[Gotcha] Deferred functions can themselves panic. Multiple deferred
panics stack up; the last one wins (after running all earlier defers).

[Go deeper] https://go.dev/blog/defer-panic-and-recover

---

## 8. Arrays and slices

Arrays are fixed-size, value-typed. You almost never use them directly.
Slices are views into underlying arrays and they are everywhere.

```go
// Array
var a [3]int = [3]int{1, 2, 3}

// Slice
var s []int                // nil slice (len=0, cap=0)
s = []int{1, 2, 3}         // literal
s = append(s, 4)           // grow (may reallocate)
s = append(s, 5, 6, 7)

// Sub-slicing - sharing the underlying array
t := s[1:3]                // [2, 3]
t[0] = 99                  // also changes s[1]

// Copy - real data duplication
u := make([]int, len(s))
copy(u, s)

// Length vs capacity
n := len(s)                // current size
c := cap(s)                // underlying array size (>= len)
```

The golden rule: **a nil slice and an empty slice behave identically
for `len`, `range`, and `append`.** Use either freely.

[Gotcha] `append` may or may not reallocate. If you hold references
to the slice and append in a loop, the new elements may or may not
be visible through the old references. Always use the returned slice
from `append` (never write `s = append(s, x)` if `s` is shared).

[Gotcha] Sub-slices keep the entire backing array alive. A 1MB slice
cut from a 1GB array holds a reference to all that memory. Use
`copy()` if you need a clean break.

[In this repo] `internal/auth/middleware.go` builds slices over slice
tricks never; we just iterate or `for ... range`.

[Go deeper] https://go.dev/blog/slices-intro

---

## 9. Maps

```go
m := map[string]int{
    "alice": 30,
    "bob":   25,
}

m["carol"] = 27

// Check before read:
age, ok := m["alice"]
if !ok {
    // not present
}

// Delete - safe even if key missing
delete(m, "bob")

// Iterate (order is intentionally randomised!)
for k, v := range m {
    fmt.Println(k, v)
}
```

A nil map behaves like an empty map for `range` and `len`, but a write
to a nil map panics. Make it real if you'll write to it:

```go
m := map[string]int{}            // empty but writable
// or
m := make(map[string]int, 16)    // pre-sized
```

[Gotcha] Map iteration order is randomised on purpose - your code
must not depend on it. This applies to tests too. If a test fails
intermittently because of map order, fix the test, not the map.

[Gotcha] `delete(m, "missing")` does nothing and returns no value.
Useful for "remove if present" code.

[Gotcha] Maps are not safe for concurrent use. Wrap them with a
mutex, or use `sync.Map`, or store them per-goroutine.

[Go deeper] https://go.dev/blog/maps

---

## 10. Structs and methods

A struct is a typed collection of fields. Methods on a struct use
**receivers**.

```go
package main

import (
    "fmt"
    "time"
)

type Ticket struct {
    EventID  int64
    UserID   string
    Quantity int
    PlacedAt time.Time
}

// Value receiver: read-only, doesn't mutate original.
func (t Ticket) Summary() string {
    return fmt.Sprintf("event=%d user=%s qty=%d",
        t.EventID, t.UserID, t.Quantity)
}

// Pointer receiver: can mutate, avoids copying.
func (t *Ticket) Cancel() {
    t.Quantity = 0
}

func main() {
    t := Ticket{EventID: 1, UserID: "u1", Quantity: 2}
    fmt.Println(t.Summary())
    t.Cancel()
    fmt.Println(t.Quantity)
}
```

Rules of thumb:

- Pick one (value vs pointer) for a method set and stick to it.
- Pointer if the struct is large (>~64 bytes is a common bar).
- Pointer if any method mutates.
- Pointer if the struct contains a mutex - copying it would copy the
  lock state.

[In this repo] `internal/auth/jwt.go` uses a value receiver on
`Claims` because `Claims` is small and immutable from the caller's
perspective. `internal/apiutil.ErrorBody` is a tiny DTO with no methods.

[Gotcha] A method on `*T` and a method on `T` are two different
method sets. If you call them through an interface or an untyped
variable, the compiler will refuse if the method is missing from the
concrete receiver type.

[Go deeper] https://go.dev/ref/spec#Method_declarations

---

## 11. Pointers

Two operators: `&` takes the address; `*` dereferences.

```go
x := 10
p := &x                  // *int pointing to x
*p = 20                  // x is now 20

y := new(int)            // *int pointing to a zero int
*y = 30                  // safe; no nil dereference
```

Pointer rules in Go:

- `nil` is the zero value; dereferencing it panics.
- Functions can take pointer parameters (`*T`) or value parameters
  (`T`). Use pointers to mutate or to avoid copying.
- Unlike C, there is no pointer arithmetic.
- Escape analysis decides whether a value goes on the heap or stack.
  You usually don't have to care; sometimes you might.

```go
// Returns a pointer because the caller needs to mutate.
func newCounter() *int {
    n := 0
    return &n          // `n` escapes to the heap; that's fine
}

func main() {
    c := newCounter()
    *c++
    fmt.Println(*c)
}
```

[Gotcha] Structs that embed `sync.Mutex` should always be passed by
pointer; copying a mutex breaks its lock state.

[Gotcha] `new(T)` is equivalent to `var x T; return &x` - useful for
non-composite types but rarely idiomatic.

---

## 12. Strings, bytes, runes

`string` is an immutable sequence of bytes, NOT of Unicode code points.
For text, convert to `[]rune` when you need code-point or grapheme
semantics.

```go
s := "héllo"
fmt.Println(len(s))                  // 6 (the é takes 2 bytes in UTF-8)
fmt.Println(len([]rune(s)))          // 5 runes

// Range over string yields runes, not bytes:
for i, r := range s {
    fmt.Printf("%d: %c (%#x)\n", i, r, r)
}

// Convert explicitly:
runes := []rune(s)                   // for code-point work
back  := string(runes)

// strconv helpers:
n, err := strconv.Atoi("42")         // string -> int
s := strconv.Itoa(42)                // int -> string
b, err := strconv.ParseBool("true")  // string -> bool
```

Three common byte/string mistakes:

1. `len(s)` counts bytes, not characters.
2. Indexing `s[i]` returns the i-th **byte**, not the i-th rune.
3. Building a string by repeated concatenation is O(n^2); use
   `strings.Builder`.

```go
var b strings.Builder
for i, v := range items {
    if i > 0 {
        b.WriteString(", ")
    }
    b.WriteString(v)
}
out := b.String()
```

[In this repo] `internal/redis` is full of byte streams between Go and
Redis. We use `redisclient` (the alias for the library) to keep Go
types out of the conversion math.

[Go deeper] https://go.dev/blog/strings

---

## 13. Printing and logging

Two packages for output:

| Need                                    | Use                  |
| --------------------------------------- | -------------------- |
| Quick debug, formatted output           | `fmt`                |
| Production logs                         | `log` (stdlib)       |
| Structured (key=value or JSON)          | `log/slog` (1.21+)   |

### `fmt`

```go
fmt.Printf("got %d items in %s\n", len(items), category)
fmt.Sprintf("got %d items in %s", len(items), category)
fmt.Println("done")
fmt.Fprintf(os.Stderr, "warn: ...")
```

### `log`

```go
log.SetFlags(log.LstdFlags | log.Lmicroseconds)
log.Print("server starting")
log.Printf("listening on %s", addr)
log.Fatalf("could not bind: %v", err)        // prints + os.Exit(1)
log.Fatalf("fatal: %v", err)                 // ditto
```

### `log/slog` (structured, modern)

```go
import "log/slog"

logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
logger.Info("listening", "addr", addr, "pid", os.Getpid())
// -> {"time":"...","level":"INFO","msg":"listening","addr":":8080","pid":12345}
```

[In this repo] `cmd/api/main.go` uses `log.SetFlags(log.LstdFlags |
log.Lmicroseconds)` and `log.Printf` everywhere. For the high-volume
production version we'd switch to `slog` so log lines could be parsed
structurally.

[Go deeper] https://pkg.go.dev/log/slog

---

## 14. Comments and godoc

Two flavours:

```go
// single-line comment
// doc comments START with the name of the thing they document.
// they appear on godoc and in IDE tooltips.

/*
   Block comment. Less common, mostly used to comment out code.
*/
```

Doc comment rules:

- Start with the name of the declared thing:
  `// Issue signs a new JWT ...` not `// This function issues ...`.
- One blank line before, no blank line at the end.
- For packages: a doc comment on the `package` line. Most projects
  put it in a file named `doc.go`.

```go
// Package auth implements the lite auth model: HS256 JWT ...
package auth
```

Run `go doc github.com/emmitt-k/ticket-deal/internal/auth` to read
the docs in a shell.

[In this repo] Every exported func, type, and package in `internal/auth`,
`internal/config`, and `internal/apiutil` has a doc comment. Run
`go doc ./...` to see.

[Gotcha] Unexported things don't need godoc-style comments, but good
projects still document anything that's not trivially obvious.

[Go deeper] https://go.dev/blog/godoc

---

# Part 2 - Building Blocks

## 15. Interfaces (small ones)

Interfaces are satisfied **implicitly**. You don't write `implements`.
You write a method, and if it matches, your struct fits.

> "Accept interfaces, return concrete types." - Go proverb

```go
package main

import (
    "fmt"
    "strings"
)

type Stringer interface {
    String() string
}

type upper string

func (u upper) String() string { return strings.ToUpper(string(u)) }

func printIt(s Stringer) {
    fmt.Println("got:", s.String())
}

func main() {
    printIt(upper("hello"))   // GOT: HELLO
}
```

The smaller the interface, the better. Standard examples include:

- `io.Reader` (`Read(p []byte) (n int, err error)`)
- `io.Writer` (`Write(p []byte) (n int, err error)`)
- `error` (`Error() string`)
- `fmt.Stringer` (`String() string`)
- `sort.Interface` (3 methods: Len, Less, Swap)

A "one-method interface" is idiomatic. A five-method interface is
usually a smell - it forces too many implementations.

[In this repo] `*jwt.Token` returned by `golang-jwt/jwt/v5` already
satisfies `Stringer`, `error`, etc. We don't define our own interfaces
in the auth package - the standard `error` type is all we need.

[Gotcha] `interface{}` (or its alias `any`, see Section 18) is escape
valve, not a design pattern. Use it when you must accept heterogeneous
data; reach for generics instead when you can.

[Gotcha] Embedded interfaces compose them. Defining a new interface
that embeds `io.Reader` and `io.Closer` makes a `ReadCloser`. This is
not Python-style inheritance - it is composition.

[Go deeper] https://go.dev/ref/spec#Interface_types

---

## 16. Composition via embedding

Go has no inheritance. Instead you embed one type inside another, and
its methods are "promoted" to the outer type.

```go
type Logger struct{ ... }
func (l Logger) Log(msg string) { ... }

type Server struct {
    Logger                              // promoted
    Addr string
}

// Server now has a .Log method, called like s.Log("hi").
```

Embedding an interface lets you swap implementations easily:

```go
type Server struct {
    Logger logger                          // interface, not struct
}
// In production you plug in a real logger; in tests, a fake.
```

Three patterns at the language level:

| Pattern                | What it gives you                                   |
| ---------------------- | --------------------------------------------------- |
| Struct embeds struct   | Field access + promoted methods                      |
| Struct embeds interface | Caller plugs in any implementation                  |
| Interface embeds interface | Wider, narrower contract                       |

[In this repo] No embedded interfaces yet; the redis package uses
direct `*redis.Client` because we never need to substitute.

[Gotcha] Promotion does NOT let the outer type *override* an inner
method with the same name. If you embed two things that both define
`Foo`, the outer struct can't reach either cleanly; explicit
re-declaration wins, but only by shadowing.

---

## 17. Type assertions and switches

A type assertion reveals the dynamic type stored in an interface:

```go
var v any = "hello"

s, ok := v.(string)
if !ok {
    // not a string
}

// "comma-ok" form is safe; without comma it panics on mismatch
s := v.(string)         // panics if v isn't string
```

For branching on type, use a type switch:

```go
switch x := v.(type) {
case nil:
    fmt.Println("nil")
case string:
    fmt.Println("string:", x)
case int:
    fmt.Println("int:", x)
default:
    fmt.Println("other")
}
```

You'll see type assertions a lot at the seam between typed Go code
and untyped libraries (encoding/json, reflect, etc.). When you find
yourself writing a lot of `switch x.(type)` in business logic, stop
and reach for generics (Section 29) or a better design.

[In this repo] `golang-jwt/jwt/v5` returns `any` for token data in a
few places; we don't currently need type assertions because we use the
typed `Claims` struct from Section 19.

[Gotcha] `x, ok := v.(T)` is the safe form. Without `ok`, the
program **panics** on mismatch. Reserve un-keyed assertions for code
where the type is provably correct (e.g. immediately after a
`switch v.(type)`).

---

## 18. The empty interface and `any`

`interface{}` is the interface with zero methods - which means every
type satisfies it. In Go 1.18+ it's aliased as `any`.

```go
var x any
x = 42
x = "hello"
x = []int{1, 2, 3}
```

When to use:

- You genuinely don't know the type at compile time (e.g. JSON values).
- The function is generic in spirit but pre-generics.
- It's the established API of a stdlib package.

When NOT to use:

- "I want to accept any of these three types." Use generics or a
  union-typed input.
- "It's convenient." Add a struct field or a new parameter; you'll
  get the type safety back.

```go
// Reasonable: heterogeneous map value.
func diff(a, b map[string]any) (added []string) { ... }

// Smelly: a function that "accepts" whatever the caller gives.
// Often indicates the function should be redesigned.
```

[In this repo] We use `any` in:

- `slog` calls (Section 13)
- Variadic arguments (`args ...any`)
- Test cases that need to mix strings and numbers in a single slice

---

## 19. JSON encoding

`encoding/json` is the de-facto standard. Two functions: `Marshal`
(Go -> bytes) and `Unmarshal` (bytes -> Go).

```go
package main

import (
    "encoding/json"
    "fmt"
)

type Event struct {
    ID    int64  `json:"id"`
    Name  string `json:"name"`
    Stock int    `json:"stock"`
}

func main() {
    // Marshal
    e := Event{ID: 1, Name: "Drop", Stock: 100}
    b, _ := json.Marshal(e)
    fmt.Println(string(b))
    // {"id":1,"name":"Drop","stock":100}

    // Unmarshal
    var f Event
    _ = json.Unmarshal([]byte(`{"id":2,"name":"Pre-Sale"}`), &f)
    fmt.Println(f)
}
```

Field tags control the wire form. Conventions:

- `json:"name"` - lowercase, snake_case preferred for public APIs
- `json:"-"` - never serialize
- `json:",omitempty"` - drop if zero
- `json:"id,string"` - encode numbers as strings (rare; useful for
  preserving precision in JavaScript clients)

[In this repo] `internal/apiutil.ErrorBody` is the JSON shape of every
non-2xx response. `internal/config/config.go` reads env vars, not
JSON. The events and reservations tables from `migrations/001_init.sql`
use `json` columns - they're read by Postgres, not by Go's
`encoding/json`.

[Gotcha] Unexported fields are ignored by `encoding/json`.
[Gotcha] `json.Marshal` on a struct that has a `time.Time` field
will encode it as RFC 3339 by default - good. If you change the time
format, encode it yourself.
[Gotcha] `json.Unmarshal` of unknown fields is silent by default;
use `json.Decoder.DisallowUnknownFields()` if that's a security
concern.

[Go deeper] https://pkg.go.dev/encoding/json

---

## 20. Testing basics

A test file lives alongside the file it tests:

- `foo.go` <-> `foo_test.go`
- Uses `_test` package suffix (sometimes; see below)

```go
// foo.go
package foo

func Double(n int) int { return n * 2 }
```

```go
// foo_test.go
package foo                       // in-package: sees unexported
// or
package foo_test                  // external: only public API

import "testing"

func TestDouble(t *testing.T) {
    got := Double(21)
    if got != 42 {
        t.Errorf("got %d, want 42", got)
    }
}
```

Common `t` methods:

| Method                | Effect                                              |
| --------------------- | --------------------------------------------------- |
| `t.Error(args...)`    | Mark failed; continue                               |
| `t.Fatalf(args...)`   | Mark failed; stop the test                          |
| `t.Skip("reason")`    | Skip - useful when an external dep is missing       |
| `t.Helper()`          | Mark this function as a helper; stack traces stop here |
| `t.Cleanup(func)`     | Schedule teardown after this test                   |
| `t.Run("name", sub)`  | Run a sub-test                                      |
| `t.Parallel()`        | Run alongside other parallel tests                  |

[In this repo] Both files in `internal/auth/` use `package auth_test`
(external) - the entire auth surface is exported, so there's no
reason to be in-package. `internal/redis/*.go` is in-package because
helpers like `newTestClient` need to call unexported internals.

[Gotcha] A test file with no `Test` or `Benchmark` functions will be
flagged by `go vet`. If you want a file with only fuzz tests, name at
least one `TestX`.

[Gotcha] `t.Parallel()` should come BEFORE any setup that captures `t`.

[Go deeper] https://go.dev/blog/subtests

---

## 21. testify and table-driven tests

`github.com/stretchr/testify` provides `assert`, `require`, `mock`,
and `suite`. The first two are sugar over `t.Errorf` / `t.Fatalf`.

```go
package auth_test

import (
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/emmitt-k/ticket-deal/internal/auth"
)

func TestVerifyRejectsTampered(t *testing.T) {
    tok, err := auth.Issue("alice", 42, []byte("0123456789abcdef0123456789abcdef"), time.Minute)
    require.NoError(t, err)        // require: stop on failure

    tampered := tok[:len(tok)-2] + "AA"
    assert.ErrorIs(t, auth.Verify(tampered, []byte("0123456789abcdef0123456789abcdef")),
        auth.ErrTokenSignature)
}
```

A **table-driven test** multiplies a single test function across many
cases:

```go
func TestIssueRejectsBadInput(t *testing.T) {
    secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

    cases := []struct {
        name    string
        user    string
        event   int64
        secret  []byte
        ttl     time.Duration
        wantSub string
    }{
        {"empty_user", "", 1, secret, time.Minute, "userID"},
        {"zero_event", "alice", 0, secret, time.Minute, "eventID"},
        // ...
    }
    for _, tc := range cases {
        tc := tc                                 // capture for parallel
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            _, err := auth.Issue(tc.user, tc.event, tc.secret, tc.ttl)
            require.Error(t, err)
            assert.Contains(t, err.Error(), tc.wantSub)
        })
    }
}
```

[In this repo] Every test file in this repo uses `testify`. We
prefer `require` for "without this, the rest is meaningless"
cases (so the test stops early) and `assert` for "keep going
through the assertions" cases.

[Gotcha] `assert` continues after a failed assertion; `require` fails
the test immediately. Use the right one - mixing them up is a
classic bug.

[Gotcha] When ranging, capture the loop variable (`tc := tc`) if you
plan to use `t.Parallel()`. Pre-Go 1.22 had a long-standing bug
where iterations shared variables; we're on 1.22+ now so the capture
is mostly defensive style.

[Go deeper] https://pkg.go.dev/github.com/stretchr/testify

---

# Part 3 - Concurrency

## 22. Goroutines

`go f(args...)` runs `f` in the background. Goroutines are cheap -
runtime-scheduled, initially only a few KB of stack, easily
hundreds of thousands alive at once.

```go
package main

import (
    "fmt"
    "time"
)

func worker(id int, done <-chan struct{}) {
    for {
        select {
        case <-done:
            fmt.Println("worker", id, "done")
            return
        case <-time.After(100 * time.Millisecond):
            fmt.Println("worker", id, "tick")
        }
    }
}

func main() {
    done := make(chan struct{})
    for i := 0; i < 3; i++ {
        go worker(i, done)
    }
    time.Sleep(450 * time.Millisecond)
    close(done)
    time.Sleep(50 * time.Millisecond)   // let workers see the close
}
```

### Lifetime and leaks

A goroutine lives until `f` returns. If you don't give it a way to
exit, it lives forever - that's a goroutine leak.

```go
go func() {
    for {
        do()                       // no exit: leaks if do() never errors
    }
}()
```

Always include a `select` with a cancellation channel, or make the
loop's termination condition explicit.

[In this repo] `cmd/api/main.go` starts the HTTP server in a goroutine
(so the main function can keep moving), and uses
`signal.NotifyContext` to cancel it on SIGTERM. See Section 39 for the
full pattern.

[Gotcha] A goroutine that panics in absence of `recover` will crash
the whole process. The HTTP server catches panics; bare goroutines
do not. Wrap risky goroutines in `defer recover()` (but really, prefer
returning errors to a managed channel).

[Go deeper] https://go.dev/doc/effective_go#goroutines

---

## 23. Channels

A channel is a typed conduit between goroutines. Make it:

```go
ch := make(chan int)            // unbuffered
ch := make(chan int, 5)         // buffered to 5
close(ch)                       // sender signals "no more data"
```

Operations:

```go
ch <- v                         // send (blocks on unbuffered if no receiver)
v := <-ch                       // receive (blocks until send ready)
v, ok := <-ch                   // receive + check if channel is closed
for v := range ch { ... }       // iterate until close
select { ... }                  // wait on multiple channels (Section 24)
```

| Property              | Unbuffered (`make(chan T)`)         | Buffered (`make(chan T, n)`)         |
| --------------------- | ----------------------------------- | ------------------------------------- |
| Send blocks until...  | receiver is ready                   | buffer has space                     |
| Receive blocks until.. | sender sends                       | buffer has data OR channel closed     |
| Use                   | Synchronization, one-shot signals   | Producer/consumer with backpressure   |

```go
// Producer/consumer
in := make(chan int, 100)
done := make(chan struct{})

go func() {
    defer close(done)
    for x := range source {
        in <- process(x)        // blocks when buffer full → backpressure
    }
    close(in)
}()

go func() {
    for x := range in {
        consume(x)
    }
    <-done                       // wait for producer to finish
}()
```

### Wait, but is it closed?

```go
v, ok := <-ch
if !ok {
    // channel is closed and drained
}
```

Sending on a closed channel panics. Don't close channels you didn't
create.

[In this repo] `cmd/api/main.go` makes a small `errCh` (buffered,
size 1) that the server goroutine sends into if it crashes. It's not
closed because we only ever read one message.

[Gotcha] `range ch` keeps reading until `ch` is closed. If you forget
the `close(ch)`, the loop runs forever.

[Gotcha] Channel values are zero-cost reference types; passing a
channel by value still points at the same buffer.

[Go deeper] https://go.dev/blog/pipelines

---

## 24. `select` statement

A `select` waits on multiple channel operations at once. The first one
that becomes ready "wins", and its case body runs. Other cases are
left alone.

```go
select {
case v := <-ch1:
    fmt.Println("from ch1:", v)
case ch2 <- x:
    fmt.Println("sent to ch2")
case <-time.After(500 * time.Millisecond):
    fmt.Println("timeout")
case <-ctx.Done():
    fmt.Println("cancelled:", ctx.Err())
}
```

### The "two phones" mental model

Imagine sitting at a desk with two phones:
- Phone A connected to "user pressed Ctrl+C" (`ctx.Done()`)
- Phone B connected to "server crashed" (`errCh`)

You sit idle with one ear against each phone. When one rings, you
handle that case and stand up. The other call just gets dropped -
you'll never know about it.

This is exactly the pattern in `cmd/api/main.go`'s shutdown block -
`select` waiting for either a signal or a server error.

### `select` with `default`

```go
select {
case v := <-ch:
    handle(v)
default:
    // ch is not ready; do something else or fall through
}
```

This makes `select` non-blocking. Useful for polling but be careful
not to spin-busy.

[In this repo] `cmd/api/main.go`'s shutdown block is exactly this
pattern. `internal/redis/waitingroom.go` is full of small `select`s
implicitly - we call Lua scripts that atomically check-and-write, so
there's no explicit goroutine dance there.

[Go deeper] https://go.dev/ref/spec#Select_statements

---

## 25. `context.Context`

`context.Context` carries across an API boundary:
- cancellation (someone wants us to stop)
- deadlines (we must stop by this time)
- request-scoped values (e.g. user ID, trace ID)

The same context carries them all. Most APIs accept a `ctx` as their
first argument.

```go
package main

import (
    "context"
    "fmt"
    "time"
)

func main() {
    // WithCancel: cancellation explicit.
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()                  // IMPORTANT: always defer cancel

    go func() {
        time.Sleep(100 * time.Millisecond)
        cancel()                    // signal "stop"
    }()

    // WithTimeout: deadline-based.
    ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
    defer cancel2()

    select {
    case <-ctx.Done():
        fmt.Println("cancelled:", ctx.Err())   // context canceled
    case <-time.After(500 * time.Millisecond):
        fmt.Println("too late")
    }
}
```

Rules:

1. **Always defer `cancel()`.** Even if you don't propagate, it
   releases resources tied to the context.
2. **`ctx` is the first parameter** in every Go API that does I/O
   (network, disk, etc.) - except a few legacy functions.
3. **Don't store a `ctx` in a struct.** Pass it explicitly.
4. **Custom values** go through `context.WithValue`; use it sparingly,
   for request-scoped data only. Never use it as a way to pass
   configuration.

[In this repo]

- `cmd/api/main.go` uses `signal.NotifyContext(ctx, SIGINT, SIGTERM)`.
- `internal/auth/middleware.go` adds the verified `*Claims` to the
  request context using `context.WithValue` with a typed unexported
  key - this is the canonical "yes, use WithValue" case: request-
  scoped data, not configuration.
- `internal/redis/client.go`'s `Ping(ctx, c)` and friends all take
  contexts for timeouts on Redis calls.

[Gotcha] `context.Background()` is "never cancelled." Useful as the
top of a chain.

[Gotcha] Don't pass `nil` as a `ctx`. Use `context.TODO()` if you
truly don't have one (a placeholder the linter will spot for you).

[Gotcha] Values stored via `context.WithValue` are typed by their
**key** (which is also typed). The Go style is to use an unexported
named type:

```go
type ctxKey int
const claimsKey ctxKey = 1
```

[Go deeper] https://go.dev/blog/context

---

## 26. `sync.WaitGroup`, `sync.Mutex`, `sync.Once`

When channels feel like overkill, the `sync` package has primitives.

### `sync.WaitGroup`

Run a set of goroutines and wait for all of them.

```go
var wg sync.WaitGroup
for _, x := range items {
    wg.Add(1)
    go func(x int) {
        defer wg.Done()
        work(x)
    }(x)
}
wg.Wait()                          // blocks until Done() called len(items) times
```

### `sync.Mutex` / `sync.RWMutex`

Protect shared state.

```go
var (
    mu    sync.Mutex
    cache map[string]string
)

func Get(key string) string {
    mu.Lock()
    defer mu.Unlock()
    return cache[key]
}

// RWMutex lets multiple readers proceed concurrently.
var rw sync.RWMutex
func Read(k string) string  { rw.RLock(); defer rw.RUnlock(); return cache[k] }
func Write(k, v string)     { rw.Lock(); defer rw.Unlock(); cache[k] = v }
```

### `sync.Once`

Do something exactly once, even across goroutines.

```go
var (
    once     sync.Once
    instance *Client
)

func GetClient() *Client {
    once.Do(func() {
        instance = newClient()
    })
    return instance
}
```

[In this repo] `internal/redis/client.go`'s `NewClient` returns a
`*redis.Client` - the caller is responsible for sharing it. When the
API server uses it, a `sync.Once` would let us keep "one client per
process" without any channel or init-time complexity.

[Gotcha] Always copy structs that contain a `sync.Mutex` by pointer.
Copying a mutex copies its lock state.

[Gotcha] A `Mutex` locked twice on the same goroutine deadlocks.
`go vet` catches this if your code is simple; complex code may not
be flagged.

[Go deeper] https://pkg.go.dev/sync

---

## 27. `errgroup`

Standard `sync.WaitGroup` doesn't propagate errors. `golang.org/x/sync/errgroup`
does:

```go
import "golang.org/x/sync/errgroup"

var g errgroup.Group
for _, url := range urls {
    url := url
    g.Go(func() error {
        return fetch(url)
    })
}
if err := g.Wait(); err != nil {
    // at least one task failed
    log.Fatal(err)
}
```

`errgroup.WithContext` derives a context that cancels when the first
task errors:

```go
g, ctx := errgroup.WithContext(ctx)
for _, url := range urls {
    g.Go(func() error {
        return fetchCtx(ctx, url)
    })
}
```

[In this repo] Not currently used. Would be a natural fit for the
worker command if/when a single fetch handles multiple SQS messages
in parallel.

[Go deeper] https://pkg.go.dev/golang.org/x/sync/errgroup

---

## 28. Worker pools, pipelines, fan-out/fan-in

These patterns combine everything in Part 3 to do real work in
parallel.

### Pipeline

Each stage is a goroutine that pulls from one channel and pushes to
another.

```
input → [stage1] → ch1 → [stage2] → ch2 → [stage3] → output
```

```go
in := generate(items)
out := final(stage3(stage2(stage1(in))))
```

When each stage is in a separate function, they compose. Closing the
input channel cascades through.

### Worker pool

```go
jobs := make(chan Job, N)
results := make(chan Result, N)

for w := 0; w < workers; w++ {
    go worker(jobs, results)
}

for _, j := range allJobs {
    jobs <- j
}
close(jobs)

for i := 0; i < len(allJobs); i++ {
    r := <-results
    // ...
}
close(results)
```

### Fan-out, fan-in

Multiple goroutines read from the same input, send to a single merge
goroutine for results.

```
input ─┬→ [w1] ─┐
       ├→ [w2] ─┼→ merge → output
       └→ [w3] ─┘
```

```go
func merge(cs ...<-chan int) <-chan int {
    out := make(chan int)
    var wg sync.WaitGroup
    for _, c := range cs {
        wg.Add(1)
        go func(c <-chan int) {
            defer wg.Done()
            for v := range c {
                out <- v
            }
        }(c)
    }
    go func() {
        wg.Wait()
        close(out)
    }()
    return out
}
```

[In this repo] `cmd/api/main.go` is technically a single-request
pipeline. The SQS worker (Phase 6) will use a worker pool pattern to
drain the queue. Eventually the API may fan-out across event IDs.

[Go deeper] https://go.dev/blog/pipelines

---

# Part 4 - Intermediate Tools

## 29. Generics (Go 1.18+)

```go
func Map[T, U any](in []T, f func(T) U) []U {
    out := make([]U, len(in))
    for i, v := range in {
        out[i] = f(v)
    }
    return out
}

xs := []int{1, 2, 3}
ys := Map(xs, func(n int) string {
    return fmt.Sprint(n * 2)
})
// ys: ["2", "4", "6"]
```

### Constraints

```go
type Number interface {
    ~int | ~float64 | ~float32    // ~T means "underlying type T"
}

func Sum[T Number](xs []T) T {
    var s T
    for _, x := range xs {
        s += x
    }
    return s
}
```

Constraints in Go are designed to be **lightweight**. The standard
`cmp.Ordered` (Go 1.21+) and `cmp.Compare` cover most ordering needs.

[In this repo] Not currently used. Would be a natural fit for:
- A generic helper around `ReserveSeat` if we wanted to support
  multiple inventory backends.
- Type-safe wrappers around `map[string]any` if we ever drop those.

[Gotcha] Don't over-generify. Concrete types are usually fine. Add a
type parameter only when you've actually written 2-3 near-identical
implementations.

[Go deeper] https://go.dev/doc/tutorial/generics

---

## 30. `iter` package (Go 1.23+)

The `iter` package gives you `range over func`:

```go
import "iter"

func Squares(n int) iter.Seq[int] {
    return func(yield func(int) bool) {
        for i := 0; i < n; i++ {
            if !yield(i * i) {
                return
            }
        }
    }
}

for v := range Squares(5) {
    fmt.Println(v)
}
// 0, 1, 4, 9, 16
```

`iter.Seq2[K, V]` for pairs (key/value style):

```go
func Enumerate[T any](xs []T) iter.Seq2[int, T] {
    return func(yield func(int, T) bool) {
        for i, v := range xs {
            if !yield(i, v) {
                return
            }
        }
    }
}

for i, v := range Enumerate([]string{"a", "b"}) {
    fmt.Println(i, v)
}
```

Range-over-func lets you write lazy generators that integrate cleanly
with `for ... range`, `break`, and `return`.

[Go deeper] https://pkg.go.dev/iter

---

## 31. `//go:embed`

Embed files into the binary at build time. The classic example: shader
text, HTML templates, Lua scripts.

```go
package reserve

import _ "embed"

//go:embed scripts/reserve.lua
var reserveLua string

//go:embed scripts/reserve.lua scripts/token_bucket.lua
var luaScripts embed.FS

//go:embed all:static
var staticFiles embed.FS
```

[In this repo] `internal/redis/reserve.go` and
`internal/redis/waitingroom.go` use `//go:embed` to ship their Lua
scripts inside the Go binary. No external file dependencies, no
"where's this file?" deployment problems.

```go
//go:embed scripts/reserve.lua
var reserveLuaSrc string

func init() {
    ReserveScript = redis.NewScript(reserveLuaSrc)
}
```

[Gotcha] Paths in the `//go:embed` directive are relative to the
Go source file containing the directive.

[Gotcha] `//go:embed all:foo` includes files starting with `_` or `.`,
which `//go:embed foo` would skip.

[Go deeper] https://pkg.go.dev/embed

---

## 32. Reflection (use sparingly)

`reflect.Type` and `reflect.Value` let you introspect any value.

```go
package main

import (
    "fmt"
    "reflect"
)

type User struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}

func main() {
    var u User
    t := reflect.TypeOf(u)
    for i := 0; i < t.NumField(); i++ {
        f := t.Field(i)
        fmt.Printf("  %s: %s, json=%q\n",
            f.Name, f.Type, f.Tag.Get("json"))
    }
}
```

When reflection helps:

- Building a generic CLI flag parser
- An ORM or migration tool
- A custom marshaler
- A debug printer you don't want to maintain by hand

When NOT:

- You know the type at compile time - just call its methods.
- Performance matters. Reflection is slow.
- You have generics (Sections 29, 30). Use those instead.

[In this repo] Not used. `encoding/json` does the heavy lifting;
we don't need reflection on top.

[Gotcha] Reflection is unsafe in the sense that it's easy to panic.
Always use the typed forms (`reflect.Value.Field(i)` returns a
`reflect.Value` that you have to `Interface()` to use).

[Go deeper] https://pkg.go.dev/reflect

---

## 33. Build tags and OS/arch handling

Build tags conditionally include files. Top of file:

```go
//go:build linux

package main
```

The old form `// +build linux` is deprecated in favour of `//go:build`.
Some files use both for transition.

Negation, OR, AND:

```go
//go:build linux || darwin
//go:build linux,amd64
```

Within a file:

```go
package main

import "fmt"

func main() {
    if GOOS == "darwin" {
        fmt.Println("hello from macOS")
    }
    if GOARCH == "arm64" {
        fmt.Println("apple silicon!")
    }
}
```

`GOOS` and `GOARCH` are set at compile time. Both are string
constants.

[In this repo] We ship one binary; no build tags used.

[Gotcha] Build tags go in a comment block - the comment line MUST be
the topmost (no blank line above it) and MUST have a blank line
following it before any package declaration.

[Go deeper] https://pkg.go.dev/cmd/go#hdr-Build_constraints

---

## 34. Modules, workspaces, dependency hygiene

### `go.mod` & `go.sum`

```go
module github.com/emmitt-k/ticket-deal

go 1.27.1

require (
    github.com/go-chi/chi/v5 v5.3.2
)
```

`go.sum` is the cryptographic checksum file. Always commit it.
`go mod tidy` updates both.

### Vendoring

If you want to bundle deps with your code (e.g. for offline builds):

```bash
go mod vendor                   # creates vendor/
go build -mod=vendor ./...
```

This repo doesn't vendor; the module cache is enough for CI.

### `go.work` workspaces

If you have multiple Go modules you want to develop together:

```
myorg/
├── go.work
├── api/go.mod
└── worker/go.mod
```

```go
// go.work
go 1.27.1

use (
    ./api
    ./worker
)
```

Each child module can also be developed standalone. Useful when a
single repo needs multiple deployables.

### Hygiene tips

- `go mod tidy` after every change in imports.
- Commit `go.sum` always.
- Use `go get pkg@version` to bump a single dependency.
- Don't fix a version with `replace` unless you have to.
- `go list -m -u all` shows available updates.

[In this repo] `go.mod` declares the module; `go.sum` is committed
in lockstep. `tidy` is part of the verification step before any commit.

[Go deeper] https://go.dev/ref/mod

---

# Part 5 - Advanced & Practical Wisdom

## 35. The Go memory model

What guarantees exist between goroutines? Mostly, very few.

> A read of a variable `v` is *guaranteed* to observe a write to `v`
> if the read and write are ordered by the `go` statement that
> started the goroutine OR by the channel sends/receives that
> connected goroutines. - The Go Memory Model

In practice:

| Mechanism                                | Safe ordering                                |
| ---------------------------------------- | -------------------------------------------- |
| Goroutine creation                       | All writes before `go f()` are visible to `f` |
| Channel send/receive                     | Send before matching receive                 |
| `sync.Mutex` Lock/Unlock                 | Unlock before subsequent Lock                |
| `atomic` operations                      | Memory-order annotations on the op           |

Everything else: you can't assume.

### Practical rules

1. Don't share mutable state between goroutines.
2. If you must, protect it with a mutex OR pass it through channels
   (one owner at a time).
3. `go test -race` catches the most common bugs. Run it in CI.

```go
// Safe: each goroutine has its own copy.
for i := 0; i < 10; i++ {
    i := i                                  // capture, even pre-1.22
    go func(n int) {
        fmt.Println(n)
    }(i)
}

// Safe: protected by mutex.
var (
    mu sync.Mutex
    n  int
)
for i := 0; i < 10; i++ {
    go func() {
        mu.Lock()
        defer mu.Unlock()
        n++                                   // safe under mutex
    }()
}

// UNSAFE: data race (no synchronization).
var n int
for i := 0; i < 10; i++ {
    go func() { n++ }()
}
```

The third example races. `go test -race` would catch it.

[Go deeper] https://go.dev/ref/mem

---

## 36. Detecting data races with `-race`

The race detector instruments memory accesses at compile time and
keeps a happens-before graph at runtime. If two accesses to the
same word don't have a happens-before edge between them, it's a race.

```bash
go test -race ./...
go run -race ./cmd/api
```

What it catches:

- Concurrent unsynchronized reads/writes
- Channel sends/receives with missing ordering
- WaitGroup misuse

What it doesn't catch:

- Logical races (two goroutines that race in business terms but
  don't share memory)
- Infinite loops

Cost: ~2x slowdown, ~5x memory. Don't ship `-race` binaries, but
**always test with `-race`**.

[In this repo] Every test in this repo is run with `-race`. The CI
verification on every commit includes `go test -race ./...`. If
`-race` ever passes but production breaks, that's a real bug in the
test setup, not the detector.

[Go deeper] https://go.dev/blog/race-detector

---

## 37. Goroutine and channel leaks

A leak is a goroutine (or message in a buffered channel) that never
gets used. Leaks accumulate over time; eventually your process
runs out of memory or stack.

Common causes:

```go
// 1. Receiver hangs forever because the producer stopped or panicked.
go func() {
    for v := range in {
        handle(v)                       // if `in` never closes, runs forever
    }
}()

// 2. Goroutine waits on a channel that nothing will ever send to.
done := make(chan struct{})
go func() {
    <-done                              // done is never closed → leak
    work()
}()

// 3. A pipeline without deadlines.
go fetch(url)                          // if url hangs, this hangs
```

Detection and prevention:

```go
// Use pprof goroutine profile:
//   go tool pprof http://localhost:6060/debug/pprof/goroutine
// Output lists every goroutine and where it's stuck.

// Always give long-running goroutines a context.
go func() {
    for {
        select {
        case <-ctx.Done():
            return
        case <-time.After(time.Second):
            work()
        }
    }
}()
```

[In this repo] Every long-running operation is bounded by a
`context.Context` that's cancelled on shutdown. Tests add explicit
`t.Cleanup` to close any test goroutines.

[Go deeper] https://pkg.go.dev/runtime/pprof

---

## 38. Profiling with pprof

`net/http/pprof` exposes profiles over HTTP at `/debug/pprof/`.
Add a guarded pprof endpoint only when measuring.

```go
import (
    _ "net/http/pprof"      // registers /debug/pprof/* on the default mux
    "net/http"
)

go func() {
    log.Println(http.ListenAndServe("localhost:6060", nil))
}()

// Don't expose :6060 to the public internet - bind to localhost only.
```

Capture profiles:

```bash
# CPU profile: 30 seconds of stack sampling.
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Heap profile: where's memory going?
go tool pprof http://localhost:6060/debug/pprof/heap

# Goroutine profile: who's stuck?
go tool pprof http://localhost:6060/debug/pprof/goroutine

# Block profile: who's waiting on a lock/channel?
# (requires enabling in code: runtime.SetBlockProfileRate(1))
```

Common pprof idioms:

- `top10` shows the busiest functions.
- `list funcname` shows the source of a function with annotations.
- `web` (needs Graphviz) renders a flame graph.
- `peek funcname` shows callers and callees.

For tests, you can also `go test -cpuprofile cpu.out .` and inspect
the same way.

[In this repo] We don't currently expose pprof. Phase 5/6 will, behind
a localhost-only listener, so load testing can identify hot paths.

[Gotcha] `pprof` profile files can be large. Use `--seconds=10` for
quick-and-dirty checks; 30+ for serious diagnosis.

[Go deeper] https://pkg.go.dev/net/http/pprof

---

## 39. Graceful shutdown (this repo's pattern)

What we want:

1. **Ctrl+C / `docker stop`** -> stop the server cleanly.
2. **Server itself crashes** -> main() exits non-zero, doesn't hang.
3. **In-flight requests** get a few seconds to finish.

The dance in code (annotated; full version is `cmd/api/main.go`):

```go
// Step 1: hook signals to a context.
ctx, stop := signal.NotifyContext(context.Background(),
    os.Interrupt, syscall.SIGTERM)
defer stop()

// Step 2: run the server in a goroutine (it's blocking).
errCh := make(chan error, 1)
go func() {
    if err := srv.ListenAndServe(); err != nil &&
        !errors.Is(err, http.ErrServerClosed) {
        errCh <- err
    }
}()

// Step 3: wait for either a signal OR a server error.
select {
case <-ctx.Done():
    log.Println("shutdown signal received")
case err := <-errCh:
    return err
}

// Step 4: politely ask server to drain.
shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
return srv.Shutdown(shutCtx)
```

Mental models:

- `signal.NotifyContext` says "tell me when SIGINT/SIGTERM happens,
  by cancelling this context."
- `make(chan error, 1)` is a one-message inbox for the goroutine.
- `go func() {...}()` puts `ListenAndServe` in the background; the
  main function would otherwise be stuck there forever.
- `select` is "sit on two phones at once" - whichever rings first,
  handle it. The other branch is dropped.
- `srv.Shutdown(ctx)` does two things: stop accepting new connections,
  then wait for in-flight requests to finish. The deadline on `ctx`
  is the graceful period: long enough for most requests, short enough
  that orchestrators don't kill us first.

[In this repo] `cmd/api/main.go` does exactly this. See Section 7 of
this repo (`docs/comprehensive-go.md`) for what `defer` does here.

### Containerized note

In Docker, the orchestrator sends `SIGTERM` and waits
`--stop-timeout` (typically 10s). Set your shutdown timeout shorter
than that, so you exit cleanly under your own power rather than being
killed.

`docker run --stop-timeout=30 mybinary` is a common Knob to twist.

[Go deeper] https://pkg.go.dev/net/http#Server.Shutdown

---

## 40. Common pitfalls

A non-exhaustive list of things every Go developer has been bitten by.

### Closing over loop variables (pre-1.22)

```go
for _, v := range items {
    go func() { use(v) }()           // captures the same `v` across iterations
}
```

Fix: capture explicitly (`v := v`) - or be on Go 1.22+ where the
range variable is per-iteration.

### Shadowed variables in `if`

```go
if v, err := lookup(); err == nil {
    return v                         // works
}
if err := step2(); err == nil {      // NEW `v` shadows the outer one
    return v                         // ✗ compile error
}
```

Fix: use distinct names, or pass through a single scope.

### Nil map writes

```go
var m map[string]int
m["x"] = 1                          // PANIC: assignment to entry in nil map
```

Fix: `m = make(map[string]int)` first.

### Range copy

```go
for _, x := range large {
    // x is a COPY. If `x` is a struct with a mutex or a slice, mutations
    // to those fields won't propagate to the slice's other copies.
}
```

Fix: `for i := range large` and access `large[i]` by index when you
need to mutate.

### Range over channel that never closes

```go
for v := range ch { /* never exits */ }
```

Fix: ensure `close(ch)` runs at the producer's end, or use `select`
with cancellation.

### String concatenation in a loop

```go
s := ""
for _, x := range xs { s += " " + x }   // O(n^2)
```

Fix: `strings.Builder` or `strings.Join(xs, " ")` or
`fmt.Sprintf` with `%s` and a single slice.

### Ignored errors

```go
f.Close()                                  // err is silently lost
```

Fix: `if err := f.Close(); err != nil { log.Println(...) }`. Or
`defer func() { _ = f.Close() }()` and document why it's fine.

### Floating-point equality

```go
if a == b { ... }                         // unreliable for floats
```

Fix: use a small epsilon (`math.Abs(a-b) < 1e-9`).

### Goroutine panic kills the process

```go
go func() {
    panic("oh no")                         // entire process exits
}()
```

Fix: `defer recover()` at the top of the goroutine if there's any
risk. Otherwise run risky goroutines inside an `errgroup` or HTTP
server (the server catches panics in handlers).

### Defer in a loop

```go
for _, f := range files {
    f, _ := os.Open(f)
    defer f.Close()                        // all defers stack up; memory grows
}
```

Fix: hoist into a closure or use a `sync.WaitGroup`.

### Time and `time.Now()` is not monotonic-aware across sleep

Timestamps come from two sources: wall clock (changes when NTP
adjusts) and monotonic clock (steady). `time.Now()` carries both.
Subtracting two `time.Time`s gives a `Duration` that uses the monotonic
part when available, wall-clock fallback otherwise.

This is rarely a bug, but it shows up in tests that compare
"elapsed since start".

```go
start := time.Now()
do()
elapsed := time.Since(start)
if elapsed > 100*time.Millisecond {
    t.Fail()
}
```

[In this repo] the auth tests use `mintExpiredHS256Token` to forge
"already-expired" tokens. Without it, you'd need to wait real time
- which would be both slow and flaky.

[Go deeper] https://go.dev/wiki/CommonMistakes

---

# Appendix

## A. Reading order for a quickstart

If you only have 30 minutes, read these in order:

1. Sections 1, 3, 4, 5 (Hello, types, funcs, control flow)
2. Sections 6, 8, 10 (errors, slices, structs)
3. Sections 19, 20 (JSON, tests)

That's enough to read and write 70% of the Go you'll see in this
codebase.

If you have 2 hours, add:

4. Sections 22-25 (goroutines, channels, select, context) - the
   mental model that distinguishes Go from "Java without semicolons".
5. Sections 15-17 (interfaces, embedding, type assertions).

If you have an afternoon, add:

6. Sections 28, 35, 36 (pipeline/race, race detector).
7. Section 39 - graceful shutdown (the pattern you'll copy most often).
8. Section 31 (`//go:embed`) - small but mighty, used in this repo.

If you're here for the long haul, finish Part 4 and 5. Section 38
(profiling) is the one to revisit the most once you have a real
production system to debug.

## B. Where to learn more

### Primary documentation

- [go.dev/doc](https://go.dev/doc/) - the official landing; start here.
- [go.dev/ref/spec](https://go.dev/ref/spec) - the language
  specification. Dry but authoritative.
- [pkg.go.dev](https://pkg.go.dev) - search for any standard library
  package; godoc + examples in one place.

### Effective Go and the FAQ

- [go.dev/doc/effective_go](https://go.dev/doc/effective_go) - the
  classic style guide. Read once, then keep around.
- [go.dev/doc/faq](https://go.dev/doc/faq) - answers to "why is X
  that way?" questions.

### Books

- *The Go Programming Language* (Alan A. A. Donovan, Brian W.
  Kernighan) - the canonical intro book; covers the spec under
  the assumption you already know how to program.
- *Concurrency in Go* (Katherine Cox-Buday) - focused on
  Part 3 of this document.
- *100 Go Mistakes and How to Avoid Them* (Teiva Harsanyi) -
  better organized than this Section 40.

### Talks

- "Concurrency is not parallelism" - Rob Pike, the canonical
  mental-model talk.
- "go test -race" patterns - Kavya Joshi, on the race detector's
  internals.

### Real-world source to read

Find a project you respect on GitHub and read its code top-to-bottom.
Go projects that are well-organized at production scale:

- `kubernetes/`
- `moby/moby` (Docker)
- `grafana/grafana`
- `prometheus/prometheus`

Reading others' Go is the highest-leverage way to learn it.

---

_Last touched on the day this file was added to `docs/`. Once you've
read it once, return to specific sections rather than re-reading end
to end - that's how technical documents work best._
