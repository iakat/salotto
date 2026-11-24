// salotto service worker.
//
// Two jobs, and nothing else. It shows notifications, and it focuses or opens the app when one
// is clicked. It deliberately does not cache anything: a bouncer's page is a terminal attached
// to a live socket, and a cached copy of it is a stale screen that looks live. The only offline
// behaviour worth having here is the notification the phone shows while the tab is closed,
// which is the thing this file exists for.
//
// Registered by term.js after the terminal connects, so a browser that never got a working
// terminal does not end up with a worker it did not need.

const VERSION = "salotto-sw-1";

self.addEventListener("install", () => {
  // No cache to warm, so take over as soon as possible.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

// A push carries JSON: {title, body, tag, url, badge}.
self.addEventListener("push", (event) => {
  let data = { title: "salotto", body: "" };
  if (event.data) {
    try {
      data = Object.assign(data, event.data.json());
    } catch (e) {
      // A push that is not JSON is still a notification: dropping it silently would make a
      // misconfigured sender look like a broken phone.
      data.body = event.data.text();
    }
  }

  const options = {
    body: data.body || "",
    // One notification per conversation, replaced as it changes: five separate buzzes for one
    // channel is how a user turns notifications off.
    tag: data.tag || "salotto",
    renotify: false,
    data: { url: data.url || "/" },
    icon: "/icon-192.png",
    badge: "/icon-192.png",
    silent: false,
  };
  if (data.badge && Number.isFinite(data.badge)) {
    // The Badging API: supported by Chromium, ignored elsewhere, so it is set defensively.
    if (self.navigator && self.navigator.setAppBadge) {
      self.navigator.setAppBadge(data.badge).catch(() => {});
    }
  }

  event.waitUntil(self.registration.showNotification(data.title || "salotto", options));
});

// Clicking a notification goes to the app: focus an existing window rather than opening a
// second one, because a second window is a second bouncer client and a second copy of the
// same conversation.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = (event.notification.data && event.notification.data.url) || "/";

  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
      for (const client of list) {
        if (client.url.includes(self.location.origin) && "focus" in client) {
          if ("navigate" in client) {
            client.navigate(target).catch(() => {});
          }
          return client.focus();
        }
      }
      if (self.clients.openWindow) {
        return self.clients.openWindow(target);
      }
    })
  );
});

// A push service can tell us a subscription is gone. Dropping the cache is pointless (there is
// none), so this only exists to keep the event from looking unhandled.
self.addEventListener("pushsubscriptionchange", (event) => {
  event.waitUntil(Promise.resolve());
});
