// Jest setup provided by Grafana scaffolding
import './.config/jest-setup';

// The scaffold installs Node's MessageChannel for React 19's scheduler. React 18
// (the peer of @grafana/* 13.1) then schedules through a MessagePort that keeps
// Jest from exiting; without it the scheduler falls back to setTimeout.
delete global.MessageChannel;
delete global.MessagePort;

// Combobox virtualises its menu (@tanstack/react-virtual), which renders no
// options in jsdom's zero-size layout; give elements a size so menus render.
for (const prop of ['clientHeight', 'clientWidth', 'offsetHeight', 'offsetWidth']) {
  Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, get: () => 400 });
}
