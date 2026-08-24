package telemetry

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// SetupLogger builds the one logger a service uses. Same handler, same fields,
// same shape in all three services -- that is the whole point of it living in
// telemetry next to Metrics.
func SetupLogger(service string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ // logs are written in json
		Level: slog.LevelInfo,
	}) // container' out put
	//and level info to show {"level":"INFO","msg":"order created"}
	logger := slog.New(h).With("service", service)
	//add service to the log

	/*"level": "INFO",
	  "msg": "order created",
	  "service": "orders"
	}*/
	slog.SetDefault(logger) // so package-level slog.Info() also works // si u can simply do slog.Info("order created")
	return logger
}

// to add also   "span_id": "def456..." in the log and traceid 
func LogWith(ctx context.Context) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return slog.Default()
	}
	return slog.With(
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	)
}
