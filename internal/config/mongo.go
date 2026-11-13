package config

import (
	"os"
	"strings"
)

// MongoURI names the platform database. The provisioning queue lives there, and
// this service is its writer, so it needs its own connection. Falling back to the
// platform URI keeps a deployment with one credential working without a second
// variable; the collection prefix is what separates writers.
func MongoURI() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PROVISIONER_MONGO_URI")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("NANOSEC_BUS_DATABASE_URI"))
}

// AdvertiseURL is how another replica reaches this one's internal API. Empty
// disables cross-replica tunnel forwarding.
func AdvertiseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("NANOSEC_RUNNER_ADVERTISE_URL")), "/")
}

// HTTPAddr is where the provisioner's internal API listens. The bus forwards the
// console's container actions here.
func HTTPAddr() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PROVISIONER_ADDR")); v != "" {
		return v
	}
	return ":8080"
}
