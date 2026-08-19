package telemetry

import (
	"log/slog"
	"os"
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
