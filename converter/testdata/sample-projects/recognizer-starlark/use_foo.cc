#include "foo.pb.h"

// Touches the generated message type so the #include of foo.pb.h is load-
// bearing — proves the consumer's deps edge to :foo_cc_proto supplies the
// generated header at compile time.
int use_foo_id(const recognizerstarlark::Foo &f) { return f.id(); }
