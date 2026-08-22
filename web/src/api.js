// All backend calls go through the ingress at /api, which strips the prefix
// before forwarding. The browser never talks to a pod directly, and this page
// holds no Kubernetes credentials — see the "No cluster credentials" section.

const BASE = '/api';

async function request(path, options = {}) {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    // The visitor cookie scopes notes to this browser.
    credentials: 'same-origin',
    ...options,
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (res.status === 429) {
      const retry = res.headers.get('Retry-After');
      throw new Error(
        `Too many requests. Try again in ${retry || 'a moment'}${retry ? ' seconds' : ''}.`,
      );
    }
    throw new Error(body.error || `Request failed with status ${res.status}.`);
  }
  return body;
}

export const api = {
  stats: () => request('/stats'),
  listNotes: () => request('/notes'),
  createNote: (text) =>
    request('/notes', { method: 'POST', body: JSON.stringify({ text }) }),
  deleteNote: (id) => request(`/notes/${id}`, { method: 'DELETE' }),
};

/** Short, stable label for a pod name like "app-k8s-lab-app-7ccf9f77c6-2vglv". */
export function shortPod(name) {
  if (!name) return 'unknown';
  const parts = name.split('-');
  return parts.length > 1 ? parts[parts.length - 1] : name;
}
