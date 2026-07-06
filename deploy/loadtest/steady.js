import http from "k6/http";
import { Counter, Trend } from "k6/metrics";

const ok = new Counter("responses_200");
const limited = new Counter("responses_429");
const other = new Counter("responses_other");
const okDuration = new Trend("duration_200", true);

export const options = {
  scenarios: {
    steady_under_limit: {
      executor: "constant-arrival-rate",
      rate: 8,
      timeUnit: "1s",
      duration: "30s",
      preAllocatedVUs: 20,
    },
  },
};

export default function () {
  const res = http.get("http://localhost:8080/api/users/");
  if (res.status === 200) {
    ok.add(1);
    okDuration.add(res.timings.duration);
  } else if (res.status === 429) {
    limited.add(1);
  } else {
    other.add(1);
  }
}
