'use strict';
// lodo dev's listen hook. A project's file in ~/.config/lodo/dev calls it with the project's ports and their
// addresses, and with ".<project>". A listen on a listed port with no host, localhost, 127.0.0.1, ::1, 0.0.0.0 or
// :: binds the port's address instead. Other ports, port 0, unix sockets, handles and fds stay as they are.
// ".<project>" joins VITE_HOSTS, a comma list Vite adds to server.allowedHosts, so Vite accepts the project's names
// in the Host Caddy passes through. Child processes inherit it and run the hook again, so it joins only once.
const net = require('node:net');

const LOCAL = new Set([undefined, null, '', 'localhost', '127.0.0.1', '::1', '0.0.0.0', '::']);
const VITE_HOSTS = '__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS';

module.exports = function bind(ports, hosts) {
  const cur = process.env[VITE_HOSTS] || '';
  if (hosts && !cur.split(',').includes(hosts)) process.env[VITE_HOSTS] = cur ? `${cur},${hosts}` : hosts;
  const listen = net.Server.prototype.listen;
  net.Server.prototype.listen = function (...args) {
    const a = args[0];
    const opts = typeof a === 'object' && a !== null;
    // An options object with a path, fd or handle is no TCP port.
    if (opts && (a.path != null || a.fd != null || a.handle != null || a._handle != null)) {
      return listen.apply(this, args);
    }
    const port = opts ? a.port : a;
    // Node reads a string that isn't all digits as a socket path.
    const isPort = typeof port === 'number' || (typeof port === 'string' && /^\d+$/.test(port));
    const addr = isPort ? ports[String(Number(port))] : undefined;
    const host = opts ? a.host : typeof args[1] === 'string' ? args[1] : undefined;
    if (!addr || !LOCAL.has(host)) return listen.apply(this, args);
    if (opts) return listen.call(this, { ...a, host: addr }, ...args.slice(1));
    // listen(port[, host][, backlog][, cb]): addr replaces the host (a string, null or undefined) or goes first.
    const rest = args.slice(1);
    if (rest.length && (typeof rest[0] === 'string' || rest[0] == null)) rest.shift();
    return listen.call(this, port, addr, ...rest);
  };
};
