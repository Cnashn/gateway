import http from "k6/http";
import { Counter, Trend } from "k6/metrics";

const ok = new Counter("responses_200");
const limited = new Counter("responses_429");
const other = new Counter("responses_other");
const okDuration = new Trend("duration_200", true);
const limitedDuration = new Trend("duration_429", true);

export const options = {
  scenarios: {
    burst_over_limit: {
      executor: "constant-vus",
      vus: 20,
      duration: "15s",
    },
  },
};

export default function () {
  const res = http.get("http://localhost:8080/api/orders/");
  if (res.status === 200) {
    ok.add(1);
    okDuration.add(res.timings.duration);
  } else if (res.status === 429) {
    limited.add(1);
    limitedDuration.add(res.timings.duration);
  } else {
    other.add(1);
  }
}
