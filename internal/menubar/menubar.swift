// lodo-menubar shows running local domain services in the macOS menu bar.
// Compiled by lodo into ~/.config/lodo/lodo-menubar.
import AppKit
import Darwin
import Foundation

struct DomainFile: Decodable {
	let version: Int
	let domains: [Domain]
}

struct Domain: Decodable, Equatable {
	let name: String
	let address: String
	let port: Int?
	let enabled: Bool
	let https: Bool?

	var activePort: Int { port ?? 0 }
	var isHTTPS: Bool { https ?? false }

	var urlString: String {
		(isHTTPS ? "https://" : "http://") + name
	}

	var project: String {
		let labels = name.split(separator: ".")
		if labels.count <= 2 { return name }
		return labels.suffix(2).joined(separator: ".")
	}
}

struct ProjectGroup: Equatable {
	let project: String
	let address: String
	let domains: [Domain]
}

func loadCandidates(from path: String) -> [Domain] {
	guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
	      let file = try? JSONDecoder().decode(DomainFile.self, from: data),
	      file.version == 1
	else { return [] }
	return file.domains.filter { $0.enabled && $0.activePort > 0 }
}

func groupRunning(_ domains: [Domain]) -> [ProjectGroup] {
	var groups: [ProjectGroup] = []
	var indexByProject: [String: Int] = [:]
	for d in domains {
		let proj = d.project
		if let i = indexByProject[proj] {
			let old = groups[i]
			groups[i] = ProjectGroup(project: old.project, address: old.address, domains: old.domains + [d])
		} else {
			indexByProject[proj] = groups.count
			groups.append(ProjectGroup(project: proj, address: d.address, domains: [d]))
		}
	}
	return groups
}

// isListening checks whether host:port accepts a TCP connection within timeoutMs
// using a non-blocking POSIX socket.
func isListening(host: String, port: Int, timeoutMs: Int32 = 150) -> Bool {
	guard port > 0 && port <= 65535 else { return false }
	var addr = sockaddr_in()
	addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
	addr.sin_family = sa_family_t(AF_INET)
	addr.sin_port = in_port_t(UInt16(port).bigEndian)
	guard inet_pton(AF_INET, host, &addr.sin_addr) == 1 else { return false }

	let fd = socket(AF_INET, SOCK_STREAM, 0)
	guard fd >= 0 else { return false }
	defer { close(fd) }

	let flags = fcntl(fd, F_GETFL, 0)
	_ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)

	let rc = withUnsafePointer(to: &addr) {
		$0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
			connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
		}
	}
	if rc == 0 { return true }
	guard errno == EINPROGRESS else { return false }

	var pfd = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
	guard poll(&pfd, 1, timeoutMs) > 0, (pfd.revents & Int16(POLLOUT)) != 0 else { return false }

	var err: Int32 = 0
	var len = socklen_t(MemoryLayout<Int32>.size)
	guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &len) == 0 else { return false }
	return err == 0
}

func filterRunning(_ candidates: [Domain]) -> [Domain] {
	guard !candidates.isEmpty else { return [] }
	var alive = Array(repeating: false, count: candidates.count)
	let lock = NSLock()
	DispatchQueue.concurrentPerform(iterations: candidates.count) { i in
		let d = candidates[i]
		if isListening(host: d.address, port: d.activePort) {
			lock.lock()
			alive[i] = true
			lock.unlock()
		}
	}
	return zip(candidates, alive).compactMap { $1 ? $0 : nil }
}

func formatHeader(_ group: ProjectGroup) -> String {
	"\(group.project) · \(group.address)"
}

func formatRow(_ d: Domain) -> String {
	let scheme = d.isHTTPS ? "https" : "http"
	return "● \(d.name)  (\(scheme) :\(d.activePort))"
}

func dumpMenu(domainsPath: String) -> String {
	let groups = groupRunning(filterRunning(loadCandidates(from: domainsPath)))
	if groups.isEmpty {
		return "No running services\n"
	}
	var lines: [String] = []
	for group in groups {
		lines.append(formatHeader(group))
		for d in group.domains {
			lines.append(formatRow(d))
		}
	}
	return lines.joined(separator: "\n") + "\n"
}

final class MenuController: NSObject, NSMenuDelegate {
	private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
	private let menu = NSMenu()
	private let domainsPath: String

	init(domainsPath: String) {
		self.domainsPath = domainsPath
		super.init()
		statusItem.autosaveName = "io.lodo.menubar"
		statusItem.isVisible = true
		if let btn = statusItem.button {
			if let img = NSImage(systemSymbolName: "network", accessibilityDescription: "lodo local domains") {
				img.isTemplate = true
				btn.image = img
			} else {
				btn.title = ".lodo"
			}
		}
		menu.delegate = self
		statusItem.menu = menu
	}

	func menuWillOpen(_ menu: NSMenu) {
		menu.removeAllItems()
		let running = filterRunning(loadCandidates(from: domainsPath))
		let groups = groupRunning(running)

		if groups.isEmpty {
			let empty = NSMenuItem(title: "No running services", action: nil, keyEquivalent: "")
			empty.isEnabled = false
			menu.addItem(empty)
		} else {
			for (idx, group) in groups.enumerated() {
				if idx > 0 { menu.addItem(.separator()) }
				let header = NSMenuItem(title: formatHeader(group), action: nil, keyEquivalent: "")
				header.isEnabled = false
				menu.addItem(header)

				for d in group.domains {
					let item = NSMenuItem(
						title: formatRow(d),
						action: #selector(openDomain(_:)),
						keyEquivalent: ""
					)
					item.target = self
					item.representedObject = d.urlString
					item.toolTip = "\(d.urlString) -> \(d.address):\(d.activePort) (⌥-click to copy URL)"
					menu.addItem(item)
				}
			}
		}

		menu.addItem(.separator())
		let quit = NSMenuItem(title: "Hide Menu Bar Icon", action: #selector(hideMenubar), keyEquivalent: "q")
		quit.target = self
		menu.addItem(quit)
	}

	@objc private func openDomain(_ sender: NSMenuItem) {
		guard let urlStr = sender.representedObject as? String else { return }
		let flags = NSApp.currentEvent?.modifierFlags ?? NSEvent.modifierFlags
		if flags.contains(.option) {
			NSPasteboard.general.clearContents()
			NSPasteboard.general.setString(urlStr, forType: .string)
			return
		}
		if let url = URL(string: urlStr) {
			NSWorkspace.shared.open(url)
		}
	}

	@objc private func hideMenubar() {
		// Remove the plist first so it stays hidden across logins even if
		// launchctl bootout terminates this process while waiting.
		let plist = (NSHomeDirectory() as NSString).appendingPathComponent("Library/LaunchAgents/io.lodo.menubar.plist")
		try? FileManager.default.removeItem(atPath: plist)
		let proc = Process()
		proc.executableURL = URL(fileURLWithPath: "/bin/launchctl")
		proc.arguments = ["bootout", "gui/\(getuid())/io.lodo.menubar"]
		if (try? proc.run()) != nil {
			proc.waitUntilExit()
		}
		NSApp.terminate(nil)
	}
}

func runSelfTests() -> Int32 {
	func check(_ ok: Bool, _ msg: String) {
		if !ok {
			fputs("FAIL: \(msg)\n", stderr)
			exit(1)
		}
	}

	// 1. JSON decode & candidate filtering
	let tmp = (NSTemporaryDirectory() as NSString).appendingPathComponent("lodo-menubar-test-\(getpid()).json")
	defer { try? FileManager.default.removeItem(atPath: tmp) }
	let json = """
	{"version":1,"domains":[
	  {"name":"blog.lodo","address":"127.0.0.1","enabled":true},
	  {"name":"web.blog.lodo","address":"127.0.0.1","port":3000,"enabled":true,"https":true},
	  {"name":"api.blog.lodo","address":"127.0.0.1","port":4000,"enabled":false},
	  {"name":"shop.custom","address":"127.0.0.1","port":5173,"enabled":true}
	]}
	"""
	try! json.write(toFile: tmp, atomically: true, encoding: .utf8)
	let candidates = loadCandidates(from: tmp)
	check(candidates.count == 2, "expected 2 enabled port candidates, got \(candidates.count)")
	check(candidates[0].name == "web.blog.lodo" && candidates[0].urlString == "https://web.blog.lodo", "https url")
	check(candidates[0].project == "blog.lodo", "project extraction")
	check(candidates[1].name == "shop.custom" && candidates[1].urlString == "http://shop.custom", "http url")

	// 1b. Unsupported format version is ignored
	let badVer = """
	{"version":2,"domains":[{"name":"web.blog.lodo","address":"127.0.0.1","port":3000,"enabled":true}]}
	"""
	try! badVer.write(toFile: tmp, atomically: true, encoding: .utf8)
	check(loadCandidates(from: tmp).isEmpty, "unsupported version should return no candidates")

	// 2. Grouping
	let groups = groupRunning(candidates)
	check(groups.count == 2 && groups[0].project == "blog.lodo" && groups[1].project == "shop.custom", "grouping")

	// 3. Live TCP listener check
	let serverFD = socket(AF_INET, SOCK_STREAM, 0)
	check(serverFD >= 0, "socket creation")
	defer { close(serverFD) }
	var addr = sockaddr_in()
	addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
	addr.sin_family = sa_family_t(AF_INET)
	addr.sin_port = 0
	inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr)
	let bindRC = withUnsafePointer(to: &addr) {
		$0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
			bind(serverFD, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
		}
	}
	check(bindRC == 0 && listen(serverFD, 4) == 0, "bind and listen")
	var boundLen = socklen_t(MemoryLayout<sockaddr_in>.size)
	_ = withUnsafeMutablePointer(to: &addr) {
		$0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
			getsockname(serverFD, $0, &boundLen)
		}
	}
	let openPort = Int(UInt16(bigEndian: addr.sin_port))
	check(isListening(host: "127.0.0.1", port: openPort), "open port \(openPort) should be detected as listening")

	let probeDomains = [
		Domain(name: "up.blog.lodo", address: "127.0.0.1", port: openPort, enabled: true, https: false),
		Domain(name: "down.blog.lodo", address: "127.0.0.1", port: 1, enabled: true, https: false)
	]
	let running = filterRunning(probeDomains)
	check(running.map(\.name) == ["up.blog.lodo"], "filterRunning should keep only listening port")

	return 0
}

let args = CommandLine.arguments
if args.contains("--test") {
	exit(runSelfTests())
}

let defaultDomains = (NSHomeDirectory() as NSString).appendingPathComponent(".config/lodo/domains.json")
let domainsPath = ProcessInfo.processInfo.environment["LODO_DOMAINS_JSON"] ?? defaultDomains

if args.contains("--dump") {
	print(dumpMenu(domainsPath: domainsPath), terminator: "")
	exit(0)
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let controller = MenuController(domainsPath: domainsPath)
withExtendedLifetime(controller) {
	app.run()
}
