// Tigris client-upload driver. Mirrors @tigrisdata/storage/client (TS) in vanilla JS.
// Registers an Alpine data component named "uploader" via the alpine:init event so
// it's available regardless of script load ordering (this file is a module, Alpine is not).

const PART_SIZE = 5 * 1024 * 1024;
const CONCURRENCY = 4;
const BROKER_URL = "/api/upload";
const FINALIZE_URL = "/api/upload/finalize";

async function postJSON(url, body) {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const json = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(json.error || `HTTP ${r.status}`);
  return json;
}

function putWithProgress(url, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.upload.addEventListener("progress", (e) => {
      if (e.lengthComputable) onProgress(e.loaded);
    });
    xhr.addEventListener("load", () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        const etag = (xhr.getResponseHeader("ETag") || "").replaceAll('"', "");
        resolve(etag);
      } else {
        reject(new Error(`PUT failed: ${xhr.status}`));
      }
    });
    xhr.addEventListener("error", () => reject(new Error("network error")));
    xhr.addEventListener("abort", () => reject(new Error("aborted")));
    xhr.open("PUT", url);
    xhr.send(blob);
  });
}

async function uploadMultipart(file, onProgress) {
  const init = await postJSON(BROKER_URL, {
    action: "multipart-init",
    name: file.name,
  });
  const { uploadId, key, id } = init.data;

  const totalParts = Math.max(1, Math.ceil(file.size / PART_SIZE));
  const partNums = Array.from({ length: totalParts }, (_, i) => i + 1);

  const parts = await postJSON(BROKER_URL, {
    action: "multipart-get-parts",
    name: key,
    uploadId,
    parts: partNums,
  });

  const progress = new Map();
  const reportTotal = () => {
    let sum = 0;
    for (const v of progress.values()) sum += v;
    onProgress(sum, file.size);
  };

  const tasks = parts.data.map(({ part, url }, i) => async () => {
    const start = i * PART_SIZE;
    const end = Math.min(start + PART_SIZE, file.size);
    const chunk = file.slice(start, end);
    const etag = await putWithProgress(url, chunk, (loaded) => {
      progress.set(part, loaded);
      reportTotal();
    });
    progress.set(part, end - start);
    reportTotal();
    return { [part]: etag };
  });

  // simple concurrency pool
  const partIds = [];
  let cursor = 0;
  await Promise.all(
    Array.from({ length: Math.min(CONCURRENCY, tasks.length) }, async () => {
      while (cursor < tasks.length) {
        const my = cursor++;
        partIds.push(await tasks[my]());
      }
    })
  );

  await postJSON(BROKER_URL, {
    action: "multipart-complete",
    name: key,
    uploadId,
    partIds,
  });

  return { id, key };
}

document.addEventListener("alpine:init", () => {
  window.Alpine.data("uploader", () => ({
    items: [],
    busy: false,

    stage(fileList) {
      for (const file of Array.from(fileList)) {
        this.items.push({
          id: crypto.randomUUID(),
          file,
          name: file.name,
          pct: 0,
          status: "ready",
          error: "",
        });
      }
    },

    reset() {
      if (this.busy) return;
      this.items = [];
    },

    async submit() {
      if (this.busy) return;
      this.busy = true;
      try {
        for (const item of this.items) {
          if (item.status !== "ready") continue;
          item.status = "uploading";
          try {
            const { id, key } = await uploadMultipart(item.file, (loaded, total) => {
              item.pct = total > 0 ? Math.round((loaded / total) * 100) : 0;
            });
            item.pct = 100;
            item.status = "finalizing";

            window.htmx.ajax("POST", FINALIZE_URL, {
              target: "#uploads",
              swap: "beforeend",
              values: { id, key, name: item.file.name },
            });

            item.status = "done";
          } catch (e) {
            item.status = "error";
            item.error = e.message || String(e);
          }
        }
      } finally {
        this.busy = false;
      }
    },
  }));
});
