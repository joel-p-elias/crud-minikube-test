from locust import HttpUser, constant, task


class NotesUser(HttpUser):
    wait_time = constant(1)  # 1 second pause, same as sleep(1) in the k6 script

    @task
    def crud_flow(self):
        # 1. CREATE (Locust marks any 4xx/5xx response as a failure)
        created = self.client.post(
            "/notes", json={"title": "load test", "content": "hello from locust"}, name="POST /notes"
        )
        if created.status_code != 201:
            return  # create failed: skip the rest
        note_id = created.json()["id"]
        self.wait()

        # 2. READ
        self.client.get(f"/notes/{note_id}", name="GET /notes/:id")
        self.wait()

        # 3. UPDATE
        self.client.put(
            f"/notes/{note_id}", json={"title": "updated", "content": "changed by locust"}, name="PUT /notes/:id"
        )
        self.wait()

        # 4. DELETE
        self.client.delete(f"/notes/{note_id}", name="DELETE /notes/:id")
