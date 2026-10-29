// In-memory filesystem exposing the Node.js `fs` surface that Go's js/wasm
// syscall layer calls into. Go does not talk to a browser filesystem — it
// calls globalThis.fs.<method>(...args, callback) and blocks the goroutine
// until the callback fires (see $GOROOT/src/syscall/fs_js.go, fsCall).
//
// Backing the engine with this instead of patching the engine is deliberate:
// journal.Journal holds a concrete *os.File, so an in-memory store would mean
// turning the journal into an interface. Here the real FilesystemStore, the
// real journal.jsonl and the real artifacts all work, unmodified.
(() => {
  "use strict";

  const O_RDONLY = 0, O_WRONLY = 1, O_RDWR = 2;
  const O_CREAT = 64, O_EXCL = 128, O_TRUNC = 512, O_APPEND = 1024;
  const O_DIRECTORY = 65536;

  const S_IFDIR = 0o40000, S_IFREG = 0o100000;

  function fsError(code, path) {
    const e = new Error(code + (path ? ": " + code + ", '" + path + "'" : ""));
    e.code = code;
    e.errno = -1;
    if (path !== undefined) e.path = path;
    return e;
  }

  // --- tree ---------------------------------------------------------------
  // Nodes: {kind:"dir", children:Map} | {kind:"file", bytes:Uint8Array}
  const root = { kind: "dir", children: new Map(), mode: 0o755, times: Date.now() };
  let nextIno = 2;
  root.ino = 1;

  function splitPath(p) {
    const abs = p.startsWith("/") ? p : resolvePath(p);
    return abs.split("/").filter((s) => s.length > 0);
  }

  function resolvePath(p) {
    if (p.startsWith("/")) return normalize(p);
    return normalize(cwd + "/" + p);
  }

  function normalize(p) {
    const out = [];
    for (const part of p.split("/")) {
      if (part === "" || part === ".") continue;
      if (part === "..") { out.pop(); continue; }
      out.push(part);
    }
    return "/" + out.join("/");
  }

  function lookupDir(parts) {
    let node = root;
    for (const part of parts) {
      if (node.kind !== "dir") return null;
      const child = node.children.get(part);
      if (!child) return null;
      node = child;
    }
    return node;
  }

  function lookup(parts) {
    if (parts.length === 0) return root;
    const parent = lookupDir(parts.slice(0, -1));
    if (!parent || parent.kind !== "dir") return null;
    return parent.children.get(parts[parts.length - 1]) || null;
  }

  let cwd = "/fp";
  {
    const parts = splitPath(cwd);
    let node = root;
    for (const part of parts) {
      if (!node.children.has(part)) {
        node.children.set(part, { kind: "dir", children: new Map(), mode: 0o755, times: Date.now(), ino: nextIno++ });
      }
      node = node.children.get(part);
    }
  }

  function stats(node, path) {
    const isDir = node.kind === "dir";
    const size = isDir ? 0 : node.bytes.length;
    return {
      dev: 1, ino: node.ino, nlink: 1, uid: 1000, gid: 1000, rdev: 0,
      mode: (isDir ? S_IFDIR : S_IFREG) | (node.mode & 0o777),
      size, blksize: 4096, blocks: Math.ceil(size / 512),
      atimeMs: node.times, mtimeMs: node.times, ctimeMs: node.times,
      birthtimeMs: node.times,
      isDirectory: () => isDir,
      isFile: () => !isDir,
      isSymbolicLink: () => false,
      path,
    };
  }

  // --- open files ---------------------------------------------------------
  const fds = new Map();
  let nextFd = 3;

  // stdout/stderr are written by the Go runtime through fs.writeSync(fd, buf)
  // (runtime.wasmWrite). Teeing them here is what puts the engine's own output
  // on the page: nothing in the engine knows a browser is involved.
  let outputSink = null;
  function setOutputSink(fn) { outputSink = fn; }

  // Go writes stdout a token at a time, so hand the sink whole lines: the
  // page renders a terminal, and half a line renders as half a line.
  const pending = { 1: "", 2: "" };
  function deliver(fd, chunk) {
    if (!outputSink || (fd !== 1 && fd !== 2)) return;
    pending[fd] += new TextDecoder().decode(chunk);
    const lines = pending[fd].split("\n");
    pending[fd] = lines.pop();
    for (const line of lines) outputSink(line + "\n", fd);
  }

  function flushOutput() {
    for (const fd of [1, 2]) {
      if (pending[fd]) {
        const rest = pending[fd];
        pending[fd] = "";
        if (outputSink) outputSink(rest + "\n", fd);
      }
    }
  }

  // --- fs surface ---------------------------------------------------------
  const fs = {
    constants: {
      O_RDONLY, O_WRONLY, O_RDWR, O_CREAT, O_EXCL, O_TRUNC, O_APPEND,
      O_DIRECTORY, O_SYNC: 0,
      S_IFDIR, S_IFREG,
    },

    writeSync(fd, buf) {
      const view = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
      const of = fds.get(fd);
      if (of) {
        writeAt(of, view, of.position);
        of.position += view.length;
      }
      deliver(fd, view);
      return view.length;
    },
    open(path, flags, mode, cb) {
      try {
        const parts = splitPath(path);
        let node = lookup(parts);
        const wantCreate = (flags & O_CREAT) !== 0;
        const wantExcl = (flags & O_EXCL) !== 0;
        const wantDir = (flags & O_DIRECTORY) !== 0;

        if (node && wantCreate && wantExcl) return cb(fsError("EEXIST", path));
        if (!node) {
          if (!wantCreate) return cb(fsError("ENOENT", path));
          const parent = lookupDir(parts.slice(0, -1));
          if (!parent || parent.kind !== "dir") return cb(fsError("ENOENT", path));
          node = { kind: "file", bytes: new Uint8Array(0), mode: mode & 0o777, times: Date.now(), ino: nextIno++ };
          parent.children.set(parts[parts.length - 1], node);
        }
        if (wantDir && node.kind !== "dir") return cb(fsError("ENOTDIR", path));
        if ((flags & O_TRUNC) !== 0 && node.kind === "file") {
          node.bytes = new Uint8Array(0);
          node.times = Date.now();
        }
        const append = (flags & O_APPEND) !== 0;
        const writable = (flags & (O_WRONLY | O_RDWR)) !== 0;
        const fd = nextFd++;
        fds.set(fd, {
          node, path: normalize(path),
          position: append ? node.bytes.length : 0,
          append, writable,
        });
        cb(null, fd);
      } catch (e) { cb(e); }
    },

    close(fd, cb) {
      try {
        const of = fds.get(fd);
        if (!of) return cb(fsError("EBADF"));
        fds.delete(fd);
        cb(null);
      } catch (e) { cb(e); }
    },

    read(fd, buffer, offset, length, position, cb) {
      try {
        // stdin has no source here; reporting EOF is what makes a terminal
        // gate fall through instead of hanging, and the demo never uses one.
        if (fd === 0) return cb(null, 0, buffer);
        const of = fds.get(fd);
        if (!of) return cb(fsError("EBADF"));
        if (of.node.kind === "dir") return cb(fsError("EISDIR"));
        const usePos = position === null || position === undefined ? of.position : Number(position);
        const src = of.node.bytes;
        const n = Math.max(0, Math.min(length, src.length - usePos));
        if (n > 0) buffer.set(src.subarray(usePos, usePos + n), offset);
        if (position === null || position === undefined) of.position = usePos + n;
        cb(null, n, buffer);
      } catch (e) { cb(e); }
    },

    write(fd, buffer, offset, length, position, cb) {
      try {
        const view = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
        const slice = view.subarray(offset, offset + length);

        // fds 1 and 2 belong to the process, not to this filesystem. The
        // engine's own stdout takes this path, which is how its unmodified
        // output reaches the page.
        if (fd === 1 || fd === 2) {
          deliver(fd, slice);
          return cb(null, slice.length, buffer);
        }

        const of = fds.get(fd);
        if (!of) return cb(fsError("EBADF"));
        if (!of.writable) return cb(fsError("EBADF"));
        const at = (position === null || position === undefined)
          ? (of.append ? of.node.bytes.length : of.position)
          : Number(position);
        writeAt(of, slice, at);
        if (position === null || position === undefined) of.position = at + slice.length;
        cb(null, slice.length, buffer);
      } catch (e) { cb(e); }
    },

    mkdir(path, mode, cb) {
      try {
        const parts = splitPath(path);
        const name = parts[parts.length - 1];
        const parent = lookupDir(parts.slice(0, -1));
        if (!parent || parent.kind !== "dir") return cb(fsError("ENOENT", path));
        if (parent.children.has(name)) return cb(fsError("EEXIST", path));
        parent.children.set(name, {
          kind: "dir", children: new Map(),
          mode: (typeof mode === "number" ? mode : 0o755) & 0o777,
          times: Date.now(), ino: nextIno++,
        });
        cb(null);
      } catch (e) { cb(e); }
    },

    readdir(path, cb) {
      try {
        const node = lookup(splitPath(path));
        if (!node) return cb(fsError("ENOENT", path));
        if (node.kind !== "dir") return cb(fsError("ENOTDIR", path));
        cb(null, Array.from(node.children.keys()));
      } catch (e) { cb(e); }
    },

    rmdir(path, cb) {
      try {
        const parts = splitPath(path);
        const node = lookup(parts);
        if (!node) return cb(fsError("ENOENT", path));
        if (node.kind !== "dir") return cb(fsError("ENOTDIR", path));
        if (node.children.size > 0) return cb(fsError("ENOTEMPTY", path));
        lookupDir(parts.slice(0, -1)).children.delete(parts[parts.length - 1]);
        cb(null);
      } catch (e) { cb(e); }
    },

    unlink(path, cb) {
      try {
        const parts = splitPath(path);
        const parent = lookupDir(parts.slice(0, -1));
        const node = parent && parent.children.get(parts[parts.length - 1]);
        if (!node) return cb(fsError("ENOENT", path));
        if (node.kind === "dir") return cb(fsError("EISDIR", path));
        parent.children.delete(parts[parts.length - 1]);
        cb(null);
      } catch (e) { cb(e); }
    },

    rename(from, to, cb) {
      try {
        const srcParts = splitPath(from);
        const node = lookup(srcParts);
        if (!node) return cb(fsError("ENOENT", from));
        const dstParts = splitPath(to);
        const dstParent = lookupDir(dstParts.slice(0, -1));
        if (!dstParent || dstParent.kind !== "dir") return cb(fsError("ENOENT", to));
        lookupDir(srcParts.slice(0, -1)).children.delete(srcParts[srcParts.length - 1]);
        dstParent.children.set(dstParts[dstParts.length - 1], node);
        cb(null);
      } catch (e) { cb(e); }
    },

    truncate(path, len, cb) {
      try {
        const node = lookup(splitPath(path));
        if (!node) return cb(fsError("ENOENT", path));
        if (node.kind === "dir") return cb(fsError("EISDIR", path));
        node.bytes = node.bytes.slice(0, len);
        node.times = Date.now();
        cb(null);
      } catch (e) { cb(e); }
    },

    ftruncate(fd, len, cb) {
      try {
        const of = fds.get(fd);
        if (!of) return cb(fsError("EBADF"));
        of.node.bytes = of.node.bytes.slice(0, len);
        of.node.times = Date.now();
        cb(null);
      } catch (e) { cb(e); }
    },

    chmod(path, mode, cb) { applyMode(lookup(splitPath(path)), mode, path, cb); },
    fchmod(fd, mode, cb) {
      const of = fds.get(fd);
      applyMode(of && of.node, mode, undefined, cb);
    },
    chown(path, uid, gid, cb) { cb(null); },
    fchown(fd, uid, gid, cb) { cb(null); },
    fsync(fd, cb) { cb(null); },
    utimes(path, a, m, cb) {
      const node = lookup(splitPath(path));
      if (node) node.times = Date.now();
      cb(null);
    },

    stat(path, cb) { statLike(path, cb); },
    lstat(path, cb) { statLike(path, cb); },

    // Go calls fstat right after open to find out whether it opened a
    // directory, so this one is on the hot path of every journal write.
    fstat(fd, cb) {
      try {
        const of = fds.get(fd);
        if (!of) return cb(fsError("EBADF"));
        cb(null, stats(of.node, of.path));
      } catch (e) { cb(e); }
    },

    readlink(path, cb) { cb(fsError("EINVAL", path)); },
    symlink(target, path, cb) { cb(fsError("EPERM", path)); },
    link(existing, newPath, cb) { cb(fsError("EPERM", newPath)); },
    lchown(path, uid, gid, cb) { cb(null); },
  };

  function applyMode(node, mode, path, cb) {
    if (!node) return cb(fsError("ENOENT", path));
    node.mode = mode & 0o777;
    cb(null);
  }

  function statLike(path, cb) {
    try {
      const node = lookup(splitPath(path));
      if (!node) return cb(fsError("ENOENT", path));
      cb(null, stats(node, path));
    } catch (e) { cb(e); }
  }

  function writeAt(of, slice, at) {
    const node = of.node;
    const end = at + slice.length;
    if (end > node.bytes.length) {
      const grown = new Uint8Array(end);
      grown.set(node.bytes, 0);
      node.bytes = grown;
    }
    node.bytes.set(slice, at);
    node.times = Date.now();
  }

  // --- path / process -----------------------------------------------------
  const pathShim = {
    resolve: (...args) => {
      let p = "";
      for (const a of args) {
        if (typeof a !== "string" || a.length === 0) continue;
        p = a.startsWith("/") ? a : (p === "" ? a : p + "/" + a);
      }
      return normalize(p === "" ? cwd : p);
    },
    sep: "/",
    delimiter: ":",
  };

  const processShim = {
    cwd: () => cwd,
    chdir: (p) => { cwd = normalize(p); },
    getuid: () => 1000, getgid: () => 1000,
    geteuid: () => 1000, getegid: () => 1000,
    getgroups: () => [],
    pid: 1, ppid: 0,
    umask: (m) => 0o022,
    env: {},
    argv: [],
    platform: "js",
  };

  // --- journal inspection -------------------------------------------------
  // The demo page reads the engine's own journal back out of the VFS, so what
  // it shows is the file the engine wrote — not a re-implementation of it.
  function readFileString(p) {
    const node = lookup(splitPath(p));
    if (!node || node.kind !== "file") return null;
    return new TextDecoder().decode(node.bytes);
  }

  function listTree(p, depth) {
    const node = lookup(splitPath(p));
    if (!node || node.kind !== "dir") return [];
    const out = [];
    for (const [name, child] of node.children) {
      const full = normalize(p + "/" + name);
      out.push({ path: full, dir: child.kind === "dir", size: child.kind === "file" ? child.bytes.length : 0 });
      if (child.kind === "dir" && depth > 0) out.push(...listTree(full, depth - 1));
    }
    return out;
  }

  function reset() {
    fds.clear();
    nextFd = 3;
    root.children.clear();
    cwd = "/fp";
    let node = root;
    node.children.set("fp", { kind: "dir", children: new Map(), mode: 0o755, times: Date.now(), ino: nextIno++ });
  }

  globalThis.fs = fs;
  globalThis.path = pathShim;
  globalThis.process = processShim;
  globalThis.fidespipeVFS = { setOutputSink, flushOutput, readFileString, listTree, reset, normalize };
})();
