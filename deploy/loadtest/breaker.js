import http from "k6/http";
import { Counter, Trend } from "k6/metrics";

const upstreamError = new Counter("responses_502");
const shortCircuit = new Counter("responses_503");
const other = new Counter("responses_other");
const errorDuration = new Trend("duration_502_upstream_attempt", true);
const shortCircuitDuration = new Trend("duration_503_short_circuit", true);

export const options = {
  scenarios: {
    breaker_open: {
      executor: "constant-arrival-rate",
      rate: 4,
      timeUnit: "1s",
      duration: "30s",
      preAllocatedVUs: 10,
    },
  },
};

export default function () {
  const res = http.get("http://localhost:8080/api/orders/");
  if (res.status === 502) {
    upstreamError.add(1);
    errorDuration.add(res.timings.duration);
  } else if (res.status === 503) {
    shortCircuit.add(1);
    shortCircuitDuration.add(res.timings.duration);
  } else {
    other.add(1);
  }
}
