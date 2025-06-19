# krakend-sse
KrakenD component for Server Side Events

This is a fork of https://github.com/Harsh-Naicker/krakend-sse

KrakenD did not have support for server side events. To add support, opensource contribution was made by @Harsh Naicker.
Original PR raised to krakend-ce: https://github.com/krakend/krakend-ce/pull/1004
KrakenD team responded with creation of a new krakend-sse repository.
PR raised to krakend-sse: https://github.com/krakend/krakend-sse/pull/1

Unacademy's krakend-ce is on a much older version hence we had to downgrade krakend-sse in such a way that it can be injected in krakend-ce as a middleware.

## Usage in krakend-ce
```
package krakend

import (
	// Other imports
	sse "github.com/unacademy/krakend-sse"
)

// NewHandlerFactory returns a HandlerFactory with a rate-limit and a metrics collector middleware injected
func NewHandlerFactory(logger logging.Logger, metricCollector *metrics.Metrics, rejecter jose.RejecterFactory) router.HandlerFactory {
	handlerFactory := juju.HandlerFactory
	// Other middleware stacking.

	// Wrap with SSE middleware - this should be the outermost wrapper
	// so it can intercept SSE endpoints before they go through the standard chain
	handlerFactory = sse.New(handlerFactory, logger)

	return handlerFactory
}

type handlerFactory struct{}

func (h handlerFactory) NewHandlerFactory(l logging.Logger, m *metrics.Metrics, r jose.RejecterFactory) router.HandlerFactory {
	return NewHandlerFactory(l, m, r)
}

```

This repository does not need a separate build process. 