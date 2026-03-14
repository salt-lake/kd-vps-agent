//go:build !xray

package collect

type xrayProviderStub struct{}

func (xrayProviderStub) Collect(_ *Payload) {}

func NewXrayProvider(_, _ string) MetricProvider { return xrayProviderStub{} }
