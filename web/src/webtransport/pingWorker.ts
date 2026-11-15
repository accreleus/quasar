// Ticks for the control stream's ping. A hidden tab's timers are cut to about one a minute
// (Chrome's intensive throttling), a Worker's are not, and the agent ends a client that sends
// nothing for 10 s.

const scope = self as unknown as { onmessage: ((e: { data: number }) => void) | null; postMessage(v: number): void };
let timer: ReturnType<typeof setInterval> | null = null;
scope.onmessage = (e) => {
  if (timer !== null) clearInterval(timer);
  timer = setInterval(() => scope.postMessage(0), e.data);
};
