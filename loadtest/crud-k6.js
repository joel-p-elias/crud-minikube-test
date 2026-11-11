// k6 test: every virtual user repeats Create -> Read -> Update -> Delete.
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE = __ENV.BASE_URL || 'http://127.0.0.1:51296';
const VUS = Number(__ENV.VUS || 500);

export const options =
  VUS === 1
    ? { vus: 1, iterations: 1 } // functional check: one pass through the flow
    : {
        stages: [
          { duration: '20s', target: 100 },
          { duration: '20s', target: VUS }, // ramp up
          { duration: '80s', target: VUS }, // hold
          { duration: '20s', target: 0 },   // ramp down  (total 2 minutes)
        ],
      };

const params = (name) => ({ headers: { 'Content-Type': 'application/json' }, tags: { name } });

export default function () {
  // 1. CREATE
  const created = http.post(
    `${BASE}/notes`,
    JSON.stringify({ title: 'load test', content: 'hello from k6' }),
    params('POST /notes')
  );
  check(created, { 'create -> 201': (r) => r.status === 201 });

  let id;
  try { id = created.json('id'); } catch (e) { id = null; }
  if (!id) { sleep(1); return; } // create failed: skip the rest
  sleep(1);

  // 2. READ
  const read = http.get(`${BASE}/notes/${id}`, params('GET /notes/:id'));
  check(read, { 'read -> 200': (r) => r.status === 200 });
  sleep(1);


  const updated = http.put(
    `${BASE}/notes/${id}`,
    JSON.stringify({ title: 'updated', content: 'changed by k6' }),
    params('PUT /notes/:id')
  );
  check(updated, { 'update -> 200': (r) => r.status === 200 });
  sleep(1);

  // 4. DELETE
  const deleted = http.del(`${BASE}/notes/${id}`, null, params('DELETE /notes/:id'));
  check(deleted, { 'delete -> 200': (r) => r.status === 200 });
  sleep(1);
}
