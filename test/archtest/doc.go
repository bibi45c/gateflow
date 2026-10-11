// Package archtest checks the dependency rules between FlowGate modules: a service
// or test tool may import only its own packages and pkg/, and pkg/ imports no
// service.
package archtest
