// lodo-menubar provides the native macOS menu bar GUI for lodo.
// Compiled by lodo into ~/.config/lodo/lodo-menubar.
import AppKit
import Darwin
import Foundation
import SwiftUI

// MARK: - domains.json Fallback & --dump Types

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
	let compose: String?
	let root: String?

	init(
		name: String,
		address: String,
		port: Int? = nil,
		enabled: Bool,
		https: Bool? = nil,
		compose: String? = nil,
		root: String? = nil
	) {
		self.name = name
		self.address = address
		self.port = port
		self.enabled = enabled
		self.https = https
		self.compose = compose
		self.root = root
	}

	var activePort: Int { port ?? 0 }
	var isHTTPS: Bool { https ?? false }

	var urlString: String {
		(isHTTPS ? "https://" : "http://") + name
	}

	var project: String {
		extractProject(from: name)
	}
}

struct ProjectGroup: Equatable {
	let project: String
	let address: String
	let domains: [Domain]
}

func extractProject(from name: String) -> String {
	let labels = name.split(separator: ".")
	if labels.count <= 2 { return name }
	return labels.suffix(2).joined(separator: ".")
}

func loadCandidates(from path: String) -> [Domain] {
	guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
	      let file = try? JSONDecoder().decode(DomainFile.self, from: data),
	      file.version == 1
	else { return [] }
	return file.domains.filter { $0.enabled && $0.activePort > 0 }
}

func loadAllDomains(from path: String) -> [Domain] {
	guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
	      let file = try? JSONDecoder().decode(DomainFile.self, from: data),
	      file.version == 1
	else { return [] }
	return file.domains
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

// MARK: - lodo ls --json Schema

struct LsReport: Codable, Equatable {
	let setupDone: Bool
	let caddyReady: Bool
	let nextFree: String?
	let logPath: String
	let services: [LsService]
	let domains: [LsDomain]

	enum CodingKeys: String, CodingKey {
		case setupDone = "setup_done"
		case caddyReady = "caddy_ready"
		case nextFree = "next_free"
		case logPath = "log_path"
		case services
		case domains
	}
}

struct LsService: Codable, Equatable {
	let name: String   // "caddy", "dnsmasq", "menubar", "loopback", "resolvers"
	let state: String  // "on", "off", "error"
	let detail: String
	let fix: String
}

struct LsDomain: Codable, Equatable {
	let name: String
	let project: String
	let address: String
	let port: Int
	let enabled: Bool
	let https: Bool
	let compose: String
	let root: String
	let status: String // "ok", "down", "error", "off"
	let httpsOk: Bool
	let detail: String

	enum CodingKeys: String, CodingKey {
		case name, project, address, port, enabled, https, compose, root, status
		case httpsOk = "https_ok"
		case detail
	}

	var urlString: String {
		(https ? "https://" : "http://") + name
	}

	var effectiveProject: String {
		project.isEmpty ? extractProject(from: name) : project
	}
}

struct LsProjectGroup: Equatable {
	let project: String
	let address: String
	let domains: [LsDomain]
}

// MARK: - Pure Domain & CLI Helpers

private let systemServiceNames: Set<String> = ["dnsmasq", "loopback", "resolvers", "caddy"]

func needsSystemSetup(_ report: LsReport) -> Bool {
	if !report.setupDone {
		return true
	}
	return report.services.contains { svc in
		systemServiceNames.contains(svc.name) && svc.state == "error"
	}
}

func groupLsDomains(_ domains: [LsDomain]) -> [LsProjectGroup] {
	var groups: [LsProjectGroup] = []
	var indexByProject: [String: Int] = [:]
	for d in domains {
		let proj = d.effectiveProject
		if let i = indexByProject[proj] {
			let old = groups[i]
			let combined = old.domains + [d]
			let canonicalAddr = combined.first(where: { $0.name == proj })?.address ?? old.address
			groups[i] = LsProjectGroup(project: proj, address: canonicalAddr, domains: combined)
		} else {
			indexByProject[proj] = groups.count
			groups.append(LsProjectGroup(project: proj, address: d.address, domains: [d]))
		}
	}
	return groups
}

func formatLsHeader(_ group: LsProjectGroup) -> String {
	"\(group.project) · \(group.address)"
}

func formatLsDomainRow(_ d: LsDomain) -> String {
	let badge: String
	switch d.status {
	case "ok":
		badge = "●"
	case "error":
		badge = "!"
	default:
		badge = "○"
	}

	if d.port > 0 {
		let scheme = d.https ? "https" : "http"
		if !d.enabled || d.status == "off" {
			return "\(badge) \(d.name)  (\(scheme) :\(d.port), off)"
		}
		if d.status == "down" {
			return "\(badge) \(d.name)  (\(scheme) :\(d.port), down)"
		}
		return "\(badge) \(d.name)  (\(scheme) :\(d.port))"
	} else {
		if !d.enabled || d.status == "off" {
			return "\(badge) \(d.name)  (dns, off)"
		}
		if d.status == "down" {
			return "\(badge) \(d.name)  (dns, down)"
		}
		return "\(badge) \(d.name)  (dns)"
	}
}

func formatServiceTitle(label: String, service: LsService?) -> String {
	let badge = (service?.state == "on") ? "●" : "○"
	return "\(badge) \(label)"
}

func subdomainsUnder(_ name: String, in domains: [LsDomain]) -> [LsDomain] {
	let suffix = "." + name
	return domains.filter { $0.name.hasSuffix(suffix) }
}

func parentDomain(for candidateName: String, in domains: [LsDomain]) -> LsDomain? {
	var best: LsDomain?
	for d in domains {
		let suffix = "." + d.name
		if candidateName.hasSuffix(suffix), candidateName.count > suffix.count {
			if let currentBest = best {
				if d.name.count > currentBest.name.count {
					best = d
				}
			} else {
				best = d
			}
		}
	}
	return best
}

func computeNextFreeAddress(in domains: [LsDomain]) -> String? {
	let used = Set(domains.map(\.address))
	for i in 1...50 {
		let candidate = "127.0.1.\(i)"
		if !used.contains(candidate) {
			return candidate
		}
	}
	return nil
}

func resolveDomainName(_ raw: String, parent: String? = nil) -> String {
	let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
	if let parent = parent, !parent.isEmpty {
		if trimmed.isEmpty { return "" }
		let suffix = "." + parent
		if trimmed.hasSuffix(suffix) {
			let prefix = String(trimmed.dropLast(suffix.count))
			return prefix.isEmpty ? "" : trimmed
		}
		let cleaned = trimmed.hasSuffix(".") ? String(trimmed.dropLast()) : trimmed
		if cleaned.isEmpty { return "" }
		return cleaned + suffix
	}
	if trimmed.isEmpty || trimmed.contains(".") {
		return trimmed
	}
	return trimmed + ".lodo"
}

func expandComposePath(_ raw: String, home: String = NSHomeDirectory()) -> String {
	let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
	if trimmed.isEmpty || trimmed == "none" {
		return ""
	}
	if trimmed == "~" {
		return home
	}
	if trimmed.hasPrefix("~/") {
		let rest = String(trimmed.dropFirst(2))
		return (home as NSString).appendingPathComponent(rest)
	}
	return trimmed
}

func buildFallbackReport(domainsPath: String) -> LsReport {
	let configDir = (domainsPath as NSString).deletingLastPathComponent
	let logPath = (configDir as NSString).appendingPathComponent("dnsmasq.log")
	let rawDomains = loadAllDomains(from: domainsPath)
	let lsDomains = rawDomains.map { d in
		LsDomain(
			name: d.name,
			project: d.project,
			address: d.address,
			port: d.activePort,
			enabled: d.enabled,
			https: d.isHTTPS,
			compose: d.compose ?? "",
			root: d.root ?? "",
			status: d.enabled ? (d.activePort > 0 ? "down" : "ok") : "off",
			httpsOk: d.isHTTPS,
			detail: ""
		)
	}
	let nextFree = computeNextFreeAddress(in: lsDomains)
	return LsReport(
		setupDone: true,
		caddyReady: true,
		nextFree: nextFree,
		logPath: logPath,
		services: [],
		domains: lsDomains
	)
}

// MARK: - CLI Binary Resolution & Argument Builders

struct LodoExecutable: Equatable {
	let executable: String
	let prefixArgs: [String]

	func fullArguments(for subcommandArgs: [String]) -> [String] {
		prefixArgs + subcommandArgs
	}
}

func resolveLodoExecutable(
	env: [String: String] = ProcessInfo.processInfo.environment,
	home: String = NSHomeDirectory(),
	isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }
) -> LodoExecutable {
	if let bin = env["LODO_BIN"], !bin.isEmpty, isExecutable(bin) {
		return LodoExecutable(executable: bin, prefixArgs: [])
	}
	let brewBin = "/opt/homebrew/bin/lodo"
	if isExecutable(brewBin) {
		return LodoExecutable(executable: brewBin, prefixArgs: [])
	}
	let cargoBin = (home as NSString).appendingPathComponent(".cargo/bin/lodo")
	if isExecutable(cargoBin) {
		return LodoExecutable(executable: cargoBin, prefixArgs: [])
	}
	return LodoExecutable(executable: "/usr/bin/env", prefixArgs: ["lodo"])
}

enum LodoCommand {
	static func lsJSON() -> [String] {
		["ls", "--json"]
	}

	static func service(name: String, on: Bool) -> [String] {
		["service", name, on ? "on" : "off"]
	}

	static func setEnabled(name: String, enabled: Bool) -> [String] {
		["set", name, enabled ? "--on" : "--off"]
	}

	static func setHTTPS(name: String, https: Bool) -> [String] {
		["set", name, https ? "--https" : "--no-https"]
	}

	static func addDomain(
		name: String,
		address: String,
		port: String,
		https: Bool,
		compose: String,
		home: String = NSHomeDirectory()
	) -> [String] {
		let trimmedAddr = address.trimmingCharacters(in: .whitespacesAndNewlines)
		var args = ["add", name, "--address", trimmedAddr]
		let trimmedPort = port.trimmingCharacters(in: .whitespacesAndNewlines)
		let hasPort = !trimmedPort.isEmpty && trimmedPort.lowercased() != "none"
		if hasPort {
			args += ["--port", trimmedPort]
			if https {
				args.append("--https")
			}
		}
		let expandedCompose = expandComposePath(compose, home: home)
		if !expandedCompose.isEmpty {
			args += ["--compose", expandedCompose]
		}
		return args
	}

	static func editDomain(
		originalName: String,
		newName: String,
		address: String,
		port: String,
		enabled: Bool,
		https: Bool,
		compose: String,
		home: String = NSHomeDirectory()
	) -> [String] {
		let trimmedAddr = address.trimmingCharacters(in: .whitespacesAndNewlines)
		let trimmedPort = port.trimmingCharacters(in: .whitespacesAndNewlines)
		let hasPort = !trimmedPort.isEmpty && trimmedPort.lowercased() != "none"
		let portArg = hasPort ? trimmedPort : "none"
		let effectiveHTTPS = hasPort && https
		let expandedCompose = expandComposePath(compose, home: home)
		let composeArg = expandedCompose.isEmpty ? "none" : expandedCompose

		return [
			"set",
			originalName,
			"--name", newName,
			"--address", trimmedAddr,
			"--port", portArg,
			enabled ? "--on" : "--off",
			effectiveHTTPS ? "--https" : "--no-https",
			"--compose", composeArg
		]
	}

	static func removeDomain(name: String) -> [String] {
		["rm", name]
	}

	static func prune() -> [String] {
		["prune"]
	}
}

// MARK: - Asynchronous CLI Runner

struct CommandOutput {
	let exitCode: Int32
	let stdout: Data
	let stderr: String
}

final class LodoRunner {
	private let queue = DispatchQueue(label: "io.lodo.menubar.cli", qos: .userInitiated)

	func run(_ subcommandArgs: [String], completion: @escaping (CommandOutput) -> Void) {
		queue.async {
			let resolved = resolveLodoExecutable()
			let output = Self.executeSync(
				executable: resolved.executable,
				arguments: resolved.fullArguments(for: subcommandArgs)
			)
			DispatchQueue.main.async {
				completion(output)
			}
		}
	}

	private static func executeSync(executable: String, arguments: [String]) -> CommandOutput {
		let proc = Process()
		proc.executableURL = URL(fileURLWithPath: executable)
		proc.arguments = arguments
		proc.standardInput = FileHandle.nullDevice

		let outPipe = Pipe()
		let errPipe = Pipe()
		proc.standardOutput = outPipe
		proc.standardError = errPipe

		do {
			try proc.run()
		} catch {
			return CommandOutput(
				exitCode: -1,
				stdout: Data(),
				stderr: "Failed to run \(executable): \(error.localizedDescription)"
			)
		}

		let timeoutLock = NSLock()
		var didTimeOut = false
		let watchdog = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
		watchdog.schedule(deadline: .now() + .seconds(30))
		watchdog.setEventHandler {
			timeoutLock.lock()
			didTimeOut = true
			timeoutLock.unlock()
			if proc.isRunning {
				proc.terminate()
			}
		}
		watchdog.resume()
		defer { watchdog.cancel() }

		var outData = Data()
		var errData = Data()
		let ioGroup = DispatchGroup()

		ioGroup.enter()
		DispatchQueue.global(qos: .userInitiated).async {
			outData = outPipe.fileHandleForReading.readDataToEndOfFile()
			ioGroup.leave()
		}

		ioGroup.enter()
		DispatchQueue.global(qos: .userInitiated).async {
			errData = errPipe.fileHandleForReading.readDataToEndOfFile()
			ioGroup.leave()
		}

		ioGroup.wait()
		proc.waitUntilExit()

		timeoutLock.lock()
		let timedOut = didTimeOut
		timeoutLock.unlock()
		if timedOut {
			return CommandOutput(
				exitCode: -1,
				stdout: outData,
				stderr: "lodo command timed out after 30s"
			)
		}

		let errStr = String(data: errData, encoding: .utf8)?
			.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
		return CommandOutput(
			exitCode: proc.terminationStatus,
			stdout: outData,
			stderr: errStr
		)
	}
}

// MARK: - Terminal Launcher (Non-Root Setup Helper)

func shellSingleQuote(_ value: String) -> String {
	if value.isEmpty { return "''" }
	return "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

func appleScriptEscape(_ value: String) -> String {
	value
		.replacingOccurrences(of: "\\", with: "\\\\")
		.replacingOccurrences(of: "\"", with: "\\\"")
}

func launchSetupInTerminal() {
	let resolved = resolveLodoExecutable()
	let commandParts: [String]
	if resolved.executable == "/usr/bin/env" {
		commandParts = ["lodo", "setup"]
	} else {
		commandParts = [shellSingleQuote(resolved.executable), "setup"]
	}
	let commandLine = commandParts.joined(separator: " ")

	let scriptSource = """
	tell application "Terminal"
		activate
		do script "\(appleScriptEscape(commandLine))"
	end tell
	"""
	var errorDict: NSDictionary?
	if let script = NSAppleScript(source: scriptSource) {
		_ = script.executeAndReturnError(&errorDict)
		if errorDict == nil {
			return
		}
	}

	// Fallback if Automation permission to Terminal is declined: launch a .command file.
	let scriptPath = (NSTemporaryDirectory() as NSString)
		.appendingPathComponent("lodo-setup-\(getpid()).command")
	let content = "#!/bin/zsh\n\(commandLine)\n"
	guard let data = content.data(using: .utf8) else { return }
	FileManager.default.createFile(
		atPath: scriptPath,
		contents: data,
		attributes: [.posixPermissions: 0o700]
	)
	let openProc = Process()
	openProc.executableURL = URL(fileURLWithPath: "/usr/bin/open")
	openProc.arguments = ["-a", "Terminal", scriptPath]
	try? openProc.run()
}

func activateApp() {
	if #available(macOS 14.0, *) {
		NSApp.activate()
	} else {
		NSApp.activate(ignoringOtherApps: true)
	}
}

// MARK: - SwiftUI Domain Editor Window

enum EditorMode: Equatable {
	case add
	case addSubdomain(parent: String, parentAddress: String)
	case edit(domain: LsDomain)
}

final class DomainEditorModel: ObservableObject {
	@Published var mode: EditorMode = .add
	@Published var knownDomains: [LsDomain] = []
	@Published var nextFree: String?

	@Published var nameInput: String = ""
	@Published var addressInput: String = "127.0.1.1"
	@Published var addressEdited: Bool = false
	@Published var portInput: String = ""
	@Published var https: Bool = false
	@Published var enabled: Bool = true
	@Published var composeInput: String = ""

	@Published var errorMessage: String = ""
	@Published var isSaving: Bool = false

	var isEditing: Bool {
		if case .edit = mode { return true }
		return false
	}

	var parentDomainName: String? {
		if case .addSubdomain(let parent, _) = mode {
			return parent
		}
		return nil
	}

	var windowTitle: String {
		switch mode {
		case .add:
			return "Add Domain"
		case .addSubdomain(let parent, _):
			return "Add Subdomain of \(parent)"
		case .edit(let domain):
			return "Edit \(domain.name)"
		}
	}

	var resolvedName: String {
		resolveDomainName(nameInput, parent: parentDomainName)
	}

	var hasPort: Bool {
		let trimmed = portInput.trimmingCharacters(in: .whitespacesAndNewlines)
		return !trimmed.isEmpty && trimmed.lowercased() != "none"
	}

	var effectiveNextFree: String? {
		nextFree ?? computeNextFreeAddress(in: knownDomains)
	}

	func reset(mode: EditorMode, domains: [LsDomain], nextFree: String?) {
		self.mode = mode
		self.knownDomains = domains
		self.nextFree = nextFree
		self.errorMessage = ""
		self.isSaving = false

		switch mode {
		case .add:
			self.nameInput = ""
			self.addressEdited = false
			self.addressInput = self.effectiveNextFree ?? "127.0.1.1"
			self.portInput = ""
			self.https = false
			self.enabled = true
			self.composeInput = ""

		case .addSubdomain(_, let parentAddress):
			self.nameInput = ""
			self.addressEdited = true
			self.addressInput = parentAddress
			self.portInput = ""
			self.https = false
			self.enabled = true
			self.composeInput = ""

		case .edit(let d):
			self.nameInput = d.name
			self.addressEdited = true
			self.addressInput = d.address
			self.portInput = d.port > 0 ? String(d.port) : ""
			self.https = d.https
			self.enabled = d.enabled
			self.composeInput = d.compose == "none" ? "" : d.compose
		}
	}

	func updateContext(domains: [LsDomain], nextFree: String?) {
		self.knownDomains = domains
		self.nextFree = nextFree
		prefillAddressIfNeeded()
	}

	func userUpdatedName(_ value: String) {
		nameInput = value
		errorMessage = ""
		prefillAddressIfNeeded()
	}

	func userUpdatedAddress(_ value: String) {
		addressEdited = true
		addressInput = value
		errorMessage = ""
	}

	func userUpdatedPort(_ value: String) {
		portInput = value
		errorMessage = ""
	}

	func userUpdatedCompose(_ value: String) {
		composeInput = value
		errorMessage = ""
	}

	func useNextFree() {
		guard let free = effectiveNextFree else { return }
		addressEdited = true
		addressInput = free
		errorMessage = ""
	}

	func clearCompose() {
		composeInput = ""
		errorMessage = ""
	}

	private func prefillAddressIfNeeded() {
		guard !addressEdited else { return }
		let candidate = resolvedName
		if !candidate.isEmpty, let parent = parentDomain(for: candidate, in: knownDomains) {
			addressInput = parent.address
		} else {
			addressInput = effectiveNextFree ?? "127.0.1.1"
		}
	}
}

struct DomainEditorView: View {
	@ObservedObject var model: DomainEditorModel
	let focusTrigger: Int
	let onBrowseCompose: () -> Void
	let onCancel: () -> Void
	let onSave: () -> Void

	@FocusState private var nameFocused: Bool

	private var nameBinding: Binding<String> {
		Binding(
			get: { model.nameInput },
			set: { model.userUpdatedName($0) }
		)
	}

	private var addressBinding: Binding<String> {
		Binding(
			get: { model.addressInput },
			set: { model.userUpdatedAddress($0) }
		)
	}

	private var portBinding: Binding<String> {
		Binding(
			get: { model.portInput },
			set: { model.userUpdatedPort($0) }
		)
	}

	private var httpsBinding: Binding<Bool> {
		Binding(
			get: { model.hasPort && model.https },
			set: {
				model.https = $0
				model.errorMessage = ""
			}
		)
	}

	private var composeBinding: Binding<String> {
		Binding(
			get: { model.composeInput },
			set: { model.userUpdatedCompose($0) }
		)
	}

	var body: some View {
		VStack(alignment: .leading, spacing: 14) {
			// Name Row
			formRow("Name:") {
				VStack(alignment: .leading, spacing: 4) {
					HStack(spacing: 4) {
						TextField(
							model.parentDomainName != nil ? "api" : "flowy or api.shop.lodo",
							text: nameBinding
						)
						.textFieldStyle(.roundedBorder)
						.autocorrectionDisabled()
						.focused($nameFocused)
						.disabled(model.isSaving)

						if let parent = model.parentDomainName,
						   !model.nameInput.trimmingCharacters(in: .whitespacesAndNewlines).hasSuffix("." + parent) {
							Text("." + parent)
								.font(.system(.body, design: .monospaced))
								.foregroundStyle(.secondary)
						}
					}

					namePreview
				}
			}

			// Address Row
			formRow("Address:") {
				HStack(spacing: 8) {
					TextField("127.0.1.1", text: addressBinding)
						.textFieldStyle(.roundedBorder)
						.autocorrectionDisabled()
						.disabled(model.isSaving)

					if let free = model.effectiveNextFree {
						Button("Use Next Free (\(free))") {
							model.useNextFree()
						}
						.controlSize(.small)
						.disabled(
							model.isSaving ||
							model.addressInput.trimmingCharacters(in: .whitespacesAndNewlines) == free
						)
					}
				}
			}

			// Port + HTTPS + Enabled Row
			formRow("Port:") {
				VStack(alignment: .leading, spacing: 8) {
					TextField("Optional (e.g. 3000)", text: portBinding)
						.textFieldStyle(.roundedBorder)
						.autocorrectionDisabled()
						.frame(width: 190)
						.disabled(model.isSaving)

					Toggle("HTTPS", isOn: httpsBinding)
						.disabled(!model.hasPort || model.isSaving)

					if model.isEditing {
						Toggle("Enabled", isOn: $model.enabled)
							.disabled(model.isSaving)
					}
				}
			}

			// Compose File Row
			formRow("Compose File:") {
				VStack(alignment: .leading, spacing: 6) {
					TextField("Optional (/path/to/compose.yml)", text: composeBinding)
						.textFieldStyle(.roundedBorder)
						.autocorrectionDisabled()
						.disabled(model.isSaving)

					HStack(spacing: 8) {
						Button("Browse…") {
							onBrowseCompose()
						}
						.controlSize(.small)
						.disabled(model.isSaving)

						Button("Clear") {
							model.clearCompose()
						}
						.controlSize(.small)
						.disabled(
							model.isSaving ||
							model.composeInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
						)
					}
				}
			}

			if !model.errorMessage.isEmpty {
				HStack(alignment: .top, spacing: 6) {
					Image(systemName: "exclamationmark.circle.fill")
						.foregroundStyle(.red)
					Text(model.errorMessage)
						.font(.callout)
						.foregroundStyle(.red)
						.fixedSize(horizontal: false, vertical: true)
						.textSelection(.enabled)
				}
				.padding(.top, 2)
			}

			Divider()

			HStack(spacing: 10) {
				Spacer()
				Button("Cancel") {
					onCancel()
				}
				.keyboardShortcut(.cancelAction)
				.disabled(model.isSaving)

				Button(action: onSave) {
					HStack(spacing: 6) {
						if model.isSaving {
							ProgressView()
								.controlSize(.small)
						}
						Text("Save")
					}
					.frame(minWidth: 56)
				}
				.keyboardShortcut(.defaultAction)
				.disabled(model.isSaving)
			}
		}
		.padding(20)
		.frame(width: 490)
		.id(focusTrigger)
		.onAppear {
			nameFocused = true
			DispatchQueue.main.async {
				nameFocused = true
			}
		}
	}

	@ViewBuilder
	private var namePreview: some View {
		let resolved = model.resolvedName
		if !resolved.isEmpty {
			HStack(spacing: 4) {
				Text("Resolves to:")
					.foregroundStyle(.secondary)
				Text(resolved)
					.font(.system(.caption, design: .monospaced))
					.foregroundStyle(.primary)
			}
			.font(.caption)
		} else if let parent = model.parentDomainName {
			Text("Resolves to: <subdomain>.\(parent)")
				.font(.system(.caption, design: .monospaced))
				.foregroundStyle(.secondary)
		} else {
			Text("Single label appends .lodo (e.g. flowy → flowy.lodo)")
				.font(.caption)
				.foregroundStyle(.secondary)
		}
	}

	@ViewBuilder
	private func formRow<Content: View>(
		_ label: String,
		@ViewBuilder content: () -> Content
	) -> some View {
		HStack(alignment: .firstTextBaseline, spacing: 12) {
			Text(label)
				.frame(width: 96, alignment: .trailing)
			content()
				.frame(maxWidth: .infinity, alignment: .leading)
		}
	}
}

final class YamlOpenPanelDelegate: NSObject, NSOpenSavePanelDelegate {
	func panel(_ sender: Any, shouldEnable url: URL) -> Bool {
		var isDir: ObjCBool = false
		if FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir), isDir.boolValue {
			return true
		}
		let ext = url.pathExtension.lowercased()
		return ext == "yml" || ext == "yaml"
	}
}

final class DomainEditorWindowController: NSObject, NSWindowDelegate {
	private let model = DomainEditorModel()
	private let runner: LodoRunner
	private let onSaved: (_ resolvedName: String, _ httpsEnabled: Bool) -> Void
	private let openPanelDelegate = YamlOpenPanelDelegate()
	private var window: NSWindow?
	private var hasCentered = false
	private var focusCounter = 0
	private var hostingController: NSHostingController<DomainEditorView>?

	init(
		runner: LodoRunner,
		onSaved: @escaping (_ resolvedName: String, _ httpsEnabled: Bool) -> Void
	) {
		self.runner = runner
		self.onSaved = onSaved
		super.init()
	}

	func present(mode: EditorMode, domains: [LsDomain], nextFree: String?) {
		model.reset(mode: mode, domains: domains, nextFree: nextFree)
		focusCounter &+= 1

		let win = ensureWindow()
		hostingController?.rootView = makeRootView()
		win.title = model.windowTitle
		if !hasCentered {
			win.center()
			hasCentered = true
		}
		activateApp()
		win.makeKeyAndOrderFront(nil)
		win.orderFrontRegardless()
	}

	func updateContext(domains: [LsDomain], nextFree: String?) {
		guard window?.isVisible == true else { return }
		model.updateContext(domains: domains, nextFree: nextFree)
	}

	private func makeRootView() -> DomainEditorView {
		DomainEditorView(
			model: model,
			focusTrigger: focusCounter,
			onBrowseCompose: { [weak self] in self?.browseCompose() },
			onCancel: { [weak self] in self?.closeWindow() },
			onSave: { [weak self] in self?.save() }
		)
	}

	private func ensureWindow() -> NSWindow {
		if let existing = window {
			return existing
		}
		let hc = NSHostingController(rootView: makeRootView())
		if #available(macOS 13.0, *) {
			hc.sizingOptions = [.preferredContentSize]
		}
		self.hostingController = hc

		let win = NSWindow(contentViewController: hc)
		win.styleMask = [.titled, .closable]
		win.isReleasedWhenClosed = false
		win.delegate = self
		self.window = win
		return win
	}

	private func closeWindow() {
		guard !model.isSaving else { return }
		window?.orderOut(nil)
	}

	func windowShouldClose(_ sender: NSWindow) -> Bool {
		!model.isSaving
	}

	private func browseCompose() {
		let panel = NSOpenPanel()
		panel.title = "Choose Docker Compose File"
		panel.message = "Select a .yml or .yaml Docker Compose file"
		panel.canChooseFiles = true
		panel.canChooseDirectories = false
		panel.allowsMultipleSelection = false
		panel.delegate = openPanelDelegate

		let existing = expandComposePath(model.composeInput)
		if !existing.isEmpty {
			let dir = (existing as NSString).deletingLastPathComponent
			if FileManager.default.fileExists(atPath: dir) {
				panel.directoryURL = URL(fileURLWithPath: dir, isDirectory: true)
			}
		}

		if let win = window {
			panel.beginSheetModal(for: win) { [weak self] response in
				guard response == .OK, let url = panel.url else { return }
				self?.model.userUpdatedCompose(url.path)
			}
		} else if panel.runModal() == .OK, let url = panel.url {
			model.userUpdatedCompose(url.path)
		}
	}

	private func save() {
		guard !model.isSaving else { return }
		model.errorMessage = ""

		let resolved = model.resolvedName
		let args: [String]
		let httpsRequested = model.hasPort && model.https

		switch model.mode {
		case .add, .addSubdomain:
			args = LodoCommand.addDomain(
				name: resolved,
				address: model.addressInput,
				port: model.portInput,
				https: httpsRequested,
				compose: model.composeInput
			)
		case .edit(let original):
			args = LodoCommand.editDomain(
				originalName: original.name,
				newName: resolved,
				address: model.addressInput,
				port: model.portInput,
				enabled: model.enabled,
				https: httpsRequested,
				compose: model.composeInput
			)
		}

		model.isSaving = true
		runner.run(args) { [weak self] output in
			guard let self = self else { return }
			self.model.isSaving = false
			if output.exitCode == 0 {
				self.window?.orderOut(nil)
				self.onSaved(resolved, httpsRequested)
			} else {
				let stdoutMsg = String(data: output.stdout, encoding: .utf8)?
					.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
				let msg = !output.stderr.isEmpty
					? output.stderr
					: (!stdoutMsg.isEmpty ? stdoutMsg : "Command failed (exit \(output.exitCode))")
				self.model.errorMessage = msg
			}
		}
	}
}

// MARK: - Edit Menu Installation (⌘C / ⌘V / ⌘X / ⌘A / ⌘Z)

func installMainMenu() {
	let mainMenu = NSMenu()

	let appMenuItem = NSMenuItem()
	let appMenu = NSMenu()
	appMenu.addItem(
		withTitle: "Close Window",
		action: #selector(NSWindow.performClose(_:)),
		keyEquivalent: "w"
	)
	appMenuItem.submenu = appMenu
	mainMenu.addItem(appMenuItem)

	let editMenuItem = NSMenuItem()
	let editMenu = NSMenu(title: "Edit")
	editMenu.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
	let redoItem = editMenu.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "Z")
	redoItem.keyEquivalentModifierMask = [.command, .shift]
	editMenu.addItem(.separator())
	editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
	editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
	editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
	editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
	editMenuItem.submenu = editMenu
	mainMenu.addItem(editMenuItem)

	NSApp.mainMenu = mainMenu
}

// MARK: - Menu Bar Controller

enum MenuActionPayload {
	case runSetup
	case toggleService(name: String, targetOn: Bool)
	case openURL(String)
	case copyText(String)
	case setDomainEnabled(name: String, enabled: Bool)
	case setDomainHTTPS(name: String, https: Bool)
	case addSubdomain(LsDomain)
	case editDomain(LsDomain)
	case deleteDomain(LsDomain)
	case addDomain
	case prune
	case openDNSLog
	case refresh
}

final class MenuController: NSObject, NSMenuDelegate {
	private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
	private let menu = NSMenu()
	private let domainsPath: String
	private let configDir: String
	private let runner = LodoRunner()
	private lazy var editorController = DomainEditorWindowController(
		runner: runner,
		onSaved: { [weak self] resolvedName, httpsEnabled in
			self?.requestRefresh(checkHTTPSCertFor: httpsEnabled ? resolvedName : nil)
		}
	)

	private var cachedReport: LsReport?
	private var isMenuOpen = false
	private var isRefreshing = false
	private var pendingRefreshAfterCurrent = false
	private var pendingHTTPSCertCheckDomains: Set<String> = []

	private var watchSource: DispatchSourceFileSystemObject?
	private var debounceWorkItem: DispatchWorkItem?

	init(domainsPath: String) {
		self.domainsPath = domainsPath
		self.configDir = (domainsPath as NSString).deletingLastPathComponent
		super.init()

		menu.autoenablesItems = false
		menu.delegate = self

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
		statusItem.menu = menu

		startDirectoryWatcherIfNeeded()
		requestRefresh()
	}

	deinit {
		debounceWorkItem?.cancel()
		watchSource?.cancel()
	}

	// MARK: Directory Watcher (~/.config/lodo)

	private func startDirectoryWatcherIfNeeded() {
		guard watchSource == nil else { return }
		let fd = open(configDir, O_EVTONLY)
		guard fd >= 0 else { return }

		let source = DispatchSource.makeFileSystemObjectSource(
			fileDescriptor: fd,
			eventMask: [.write, .rename, .delete],
			queue: .main
		)
		source.setEventHandler { [weak self, weak source] in
			guard let self = self else { return }
			let flags = source?.data ?? []
			if flags.contains(.delete) || flags.contains(.rename) {
				self.watchSource?.cancel()
				self.watchSource = nil
			}
			self.scheduleDebouncedRefresh()
		}
		source.setCancelHandler {
			close(fd)
		}
		watchSource = source
		source.resume()
	}

	private func scheduleDebouncedRefresh() {
		debounceWorkItem?.cancel()
		let item = DispatchWorkItem { [weak self] in
			guard let self = self else { return }
			self.startDirectoryWatcherIfNeeded()
			self.requestRefresh()
		}
		debounceWorkItem = item
		DispatchQueue.main.asyncAfter(deadline: .now() + .milliseconds(150), execute: item)
	}

	// MARK: Async State Refresh (lodo ls --json)

	private func requestRefresh(checkHTTPSCertFor certDomain: String? = nil) {
		if let certDomain = certDomain {
			pendingHTTPSCertCheckDomains.insert(certDomain)
		}
		if isRefreshing {
			pendingRefreshAfterCurrent = true
			return
		}
		isRefreshing = true
		runner.run(LodoCommand.lsJSON()) { [weak self] output in
			guard let self = self else { return }
			self.isRefreshing = false

			if output.exitCode == 0,
			   let decoded = try? JSONDecoder().decode(LsReport.self, from: output.stdout) {
				let changed = self.cachedReport != decoded
				self.cachedReport = decoded
				self.editorController.updateContext(domains: decoded.domains, nextFree: decoded.nextFree)
				if self.isMenuOpen && changed {
					self.populateMenu(with: decoded)
				}
				self.checkUntrustedCertIfNeeded(in: decoded)
			} else if self.cachedReport == nil {
				let fallback = buildFallbackReport(domainsPath: self.domainsPath)
				self.cachedReport = fallback
				self.editorController.updateContext(domains: fallback.domains, nextFree: fallback.nextFree)
				if self.isMenuOpen {
					self.populateMenu(with: fallback)
				}
			}

			if self.pendingRefreshAfterCurrent {
				self.pendingRefreshAfterCurrent = false
				self.requestRefresh()
			}
		}
	}

	private func checkUntrustedCertIfNeeded(in report: LsReport) {
		guard !pendingHTTPSCertCheckDomains.isEmpty else { return }
		let namesToCheck = pendingHTTPSCertCheckDomains
		pendingHTTPSCertCheckDomains.removeAll()

		for name in namesToCheck {
			if let dom = report.domains.first(where: { $0.name == name }),
			   dom.https,
			   dom.detail.localizedCaseInsensitiveContains("certificate isn't trusted") {
				presentUntrustedCertificateAlert(domainName: dom.name, detail: dom.detail)
				break
			}
		}
	}

	// MARK: NSMenuDelegate

	func menuWillOpen(_ menu: NSMenu) {
		isMenuOpen = true
		startDirectoryWatcherIfNeeded()
		let immediate = cachedReport ?? buildFallbackReport(domainsPath: domainsPath)
		populateMenu(with: immediate)
		requestRefresh()
	}

	func menuDidClose(_ menu: NSMenu) {
		isMenuOpen = false
	}

	// MARK: Menu Construction

	private func populateMenu(with report: LsReport) {
		menu.removeAllItems()

		// 1. System Setup Warning Banner
		if needsSystemSetup(report) {
			let warn = makeActionItem(
				title: "⚠️ System setup needed — Run lodo setup in Terminal…",
				payload: .runSetup
			)
			let problems = report.services
				.filter { systemServiceNames.contains($0.name) && $0.state == "error" && !$0.detail.isEmpty }
				.map { "\($0.name): \($0.detail)" }
			if !problems.isEmpty {
				warn.toolTip = problems.joined(separator: "\n")
			}
			menu.addItem(warn)
			menu.addItem(.separator())
		}

		// 2. Services Section
		let servicesHeader = NSMenuItem(title: "Services", action: nil, keyEquivalent: "")
		servicesHeader.isEnabled = false
		menu.addItem(servicesHeader)

		let dnsSvc = report.services.first(where: { $0.name == "dnsmasq" })
		let dnsOn = dnsSvc?.state == "on"
		let dnsItem = makeActionItem(
			title: formatServiceTitle(label: "DNS (dnsmasq)", service: dnsSvc),
			payload: .toggleService(name: "dnsmasq", targetOn: !dnsOn)
		)
		dnsItem.toolTip = (dnsSvc?.detail.isEmpty == false)
			? dnsSvc?.detail
			: "Click to turn dnsmasq \(dnsOn ? "off" : "on")"
		menu.addItem(dnsItem)

		let caddySvc = report.services.first(where: { $0.name == "caddy" })
		let caddyOn = caddySvc?.state == "on"
		let caddyItem = makeActionItem(
			title: formatServiceTitle(label: "Reverse Proxy (Caddy)", service: caddySvc),
			payload: .toggleService(name: "caddy", targetOn: !caddyOn)
		)
		caddyItem.toolTip = (caddySvc?.detail.isEmpty == false)
			? caddySvc?.detail
			: "Click to turn Caddy \(caddyOn ? "off" : "on")"
		menu.addItem(caddyItem)

		menu.addItem(.separator())

		// 3. Domains Grouped by Project
		let groups = groupLsDomains(report.domains)
		if groups.isEmpty {
			let empty = NSMenuItem(title: "No configured domains", action: nil, keyEquivalent: "")
			empty.isEnabled = false
			menu.addItem(empty)
		} else {
			for (idx, group) in groups.enumerated() {
				if idx > 0 {
					menu.addItem(.separator())
				}
				let header = NSMenuItem(title: formatLsHeader(group), action: nil, keyEquivalent: "")
				header.isEnabled = false
				menu.addItem(header)

				for d in group.domains {
					let item = NSMenuItem(title: formatLsDomainRow(d), action: nil, keyEquivalent: "")
					item.isEnabled = true
					if d.enabled && d.port > 0 {
						item.target = self
						item.action = #selector(openDomainMainClick(_:))
						item.representedObject = d.urlString
						item.toolTip = "\(d.urlString) -> \(d.address):\(d.port) (⌥-click to copy URL)"
					} else {
						item.toolTip = d.detail.isEmpty ? "\(d.name) -> \(d.address)" : d.detail
					}
					item.submenu = buildDomainSubmenu(for: d)
					menu.addItem(item)
				}
			}
		}

		// 4. Footer Items
		menu.addItem(.separator())

		menu.addItem(makeActionItem(title: "Add Domain…", payload: .addDomain, keyEquivalent: "n"))
		menu.addItem(makeActionItem(title: "Prune Missing Projects", payload: .prune))
		menu.addItem(makeActionItem(title: "Open DNS Query Log", payload: .openDNSLog))
		menu.addItem(makeActionItem(title: "Refresh Status", payload: .refresh, keyEquivalent: "r"))

		menu.addItem(.separator())

		let quit = NSMenuItem(title: "Hide Menu Bar Icon", action: #selector(hideMenubar), keyEquivalent: "q")
		quit.target = self
		quit.isEnabled = true
		menu.addItem(quit)
	}

	private func buildDomainSubmenu(for d: LsDomain) -> NSMenu {
		let sub = NSMenu(title: d.name)
		sub.autoenablesItems = false

		if !d.detail.isEmpty {
			let detailItem = NSMenuItem(title: d.detail, action: nil, keyEquivalent: "")
			detailItem.isEnabled = false
			sub.addItem(detailItem)
			sub.addItem(.separator())
		}

		if d.enabled && d.port > 0 {
			sub.addItem(makeActionItem(title: "Open in Browser", payload: .openURL(d.urlString)))
		}
		if d.port > 0 {
			sub.addItem(makeActionItem(title: "Copy URL", payload: .copyText(d.urlString)))
		}
		sub.addItem(makeActionItem(title: "Copy Address (\(d.address))", payload: .copyText(d.address)))
		sub.addItem(
			makeActionItem(
				title: "Copy DOCKER_HOST_IP=\(d.address)",
				payload: .copyText("DOCKER_HOST_IP=\(d.address)")
			)
		)

		sub.addItem(.separator())

		let enabledItem = makeActionItem(
			title: "Enabled",
			payload: .setDomainEnabled(name: d.name, enabled: !d.enabled)
		)
		enabledItem.state = d.enabled ? .on : .off
		sub.addItem(enabledItem)

		if d.port > 0 {
			let httpsItem = makeActionItem(
				title: "HTTPS",
				payload: .setDomainHTTPS(name: d.name, https: !d.https)
			)
			httpsItem.state = d.https ? .on : .off
			sub.addItem(httpsItem)
		}

		sub.addItem(.separator())

		sub.addItem(makeActionItem(title: "Add Subdomain…", payload: .addSubdomain(d)))
		sub.addItem(makeActionItem(title: "Edit…", payload: .editDomain(d)))
		sub.addItem(makeActionItem(title: "Delete…", payload: .deleteDomain(d)))

		return sub
	}

	private func makeActionItem(
		title: String,
		payload: MenuActionPayload,
		keyEquivalent: String = ""
	) -> NSMenuItem {
		let item = NSMenuItem(
			title: title,
			action: #selector(handleMenuAction(_:)),
			keyEquivalent: keyEquivalent
		)
		item.target = self
		item.isEnabled = true
		item.representedObject = payload
		return item
	}

	// MARK: Menu Actions

	@objc private func openDomainMainClick(_ sender: NSMenuItem) {
		guard let urlStr = sender.representedObject as? String else { return }
		let flags = NSEvent.modifierFlags.union(NSApp.currentEvent?.modifierFlags ?? [])
		if flags.contains(.option) {
			copyToPasteboard(urlStr)
			return
		}
		openInBrowser(urlStr)
	}

	@objc private func handleMenuAction(_ sender: NSMenuItem) {
		guard let payload = sender.representedObject as? MenuActionPayload else { return }
		switch payload {
		case .runSetup:
			launchSetupInTerminal()

		case .toggleService(let name, let targetOn):
			runMutation(LodoCommand.service(name: name, on: targetOn))

		case .openURL(let urlStr):
			openInBrowser(urlStr)

		case .copyText(let text):
			copyToPasteboard(text)

		case .setDomainEnabled(let name, let enabled):
			runMutation(LodoCommand.setEnabled(name: name, enabled: enabled))

		case .setDomainHTTPS(let name, let https):
			runMutation(
				LodoCommand.setHTTPS(name: name, https: https),
				checkHTTPSCertFor: https ? name : nil
			)

		case .addSubdomain(let domain):
			let report = cachedReport ?? buildFallbackReport(domainsPath: domainsPath)
			editorController.present(
				mode: .addSubdomain(parent: domain.name, parentAddress: domain.address),
				domains: report.domains,
				nextFree: report.nextFree
			)

		case .editDomain(let domain):
			let report = cachedReport ?? buildFallbackReport(domainsPath: domainsPath)
			editorController.present(
				mode: .edit(domain: domain),
				domains: report.domains,
				nextFree: report.nextFree
			)

		case .deleteDomain(let domain):
			confirmAndDelete(domain)

		case .addDomain:
			let report = cachedReport ?? buildFallbackReport(domainsPath: domainsPath)
			editorController.present(
				mode: .add,
				domains: report.domains,
				nextFree: report.nextFree
			)

		case .prune:
			runMutation(LodoCommand.prune())

		case .openDNSLog:
			openDNSLog()

		case .refresh:
			requestRefresh()
		}
	}

	private func runMutation(_ args: [String], checkHTTPSCertFor certDomain: String? = nil) {
		runner.run(args) { [weak self] output in
			guard let self = self else { return }
			if output.exitCode != 0 {
				let msg = output.stderr.isEmpty
					? "Command failed (exit \(output.exitCode))"
					: output.stderr
				self.presentErrorAlert(title: "lodo Command Failed", message: msg)
			}
			self.requestRefresh(checkHTTPSCertFor: output.exitCode == 0 ? certDomain : nil)
		}
	}

	private func openInBrowser(_ urlStr: String) {
		guard let url = URL(string: urlStr) else { return }
		NSWorkspace.shared.open(url)
	}

	private func copyToPasteboard(_ text: String) {
		NSPasteboard.general.clearContents()
		NSPasteboard.general.setString(text, forType: .string)
	}

	private func openDNSLog() {
		let fallbackLog = (configDir as NSString).appendingPathComponent("dnsmasq.log")
		let logPath = cachedReport?.logPath.isEmpty == false
			? cachedReport!.logPath
			: fallbackLog
		if !FileManager.default.fileExists(atPath: logPath) {
			presentErrorAlert(
				title: "DNS Query Log Not Created Yet",
				message: "No log file exists yet at \(logPath). Start dnsmasq and resolve a domain first."
			)
			return
		}
		NSWorkspace.shared.open(URL(fileURLWithPath: logPath))
	}

	private func confirmAndDelete(_ domain: LsDomain) {
		let allDomains = cachedReport?.domains ?? buildFallbackReport(domainsPath: domainsPath).domains
		let subs = subdomainsUnder(domain.name, in: allDomains)

		activateApp()
		let alert = NSAlert()
		alert.alertStyle = .warning
		alert.messageText = "Delete “\(domain.name)”?"
		if subs.isEmpty {
			alert.informativeText = "This removes \(domain.name) from lodo and updates DNS and Caddy."
		} else {
			let noun = subs.count == 1 ? "subdomain" : "subdomains"
			let names = subs.map(\.name).joined(separator: ", ")
			alert.informativeText = "This will also delete \(subs.count) \(noun) under \(domain.name): \(names)."
		}
		let deleteButton = alert.addButton(withTitle: "Delete")
		if #available(macOS 11.0, *) {
			deleteButton.hasDestructiveAction = true
		}
		alert.addButton(withTitle: "Cancel")

		if alert.runModal() == .alertFirstButtonReturn {
			runMutation(LodoCommand.removeDomain(name: domain.name))
		}
	}

	private func presentUntrustedCertificateAlert(domainName: String, detail: String) {
		activateApp()
		let alert = NSAlert()
		alert.alertStyle = .informational
		alert.messageText = "Trust Caddy’s Local HTTPS Certificate"
		alert.informativeText = """
		HTTPS is enabled for \(domainName), \(detail).

		Run lodo setup in Terminal so macOS trusts Caddy’s local root CA in your keychain.
		"""
		alert.addButton(withTitle: "Run lodo setup in Terminal")
		alert.addButton(withTitle: "Later")

		if alert.runModal() == .alertFirstButtonReturn {
			launchSetupInTerminal()
		}
	}

	private func presentErrorAlert(title: String, message: String) {
		activateApp()
		let alert = NSAlert()
		alert.alertStyle = .warning
		alert.messageText = title
		alert.informativeText = message
		alert.addButton(withTitle: "OK")
		_ = alert.runModal()
	}

	@objc private func hideMenubar() {
		// Remove the plist first so it stays hidden across logins even if
		// launchctl bootout terminates this process while waiting.
		let plist = (NSHomeDirectory() as NSString)
			.appendingPathComponent("Library/LaunchAgents/io.lodo.menubar.plist")
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

// MARK: - Self Tests (--test)

func runSelfTests() -> Int32 {
	func check(_ ok: Bool, _ msg: String) {
		if !ok {
			fputs("FAIL: \(msg)\n", stderr)
			exit(1)
		}
	}

	// 1. JSON decode & candidate filtering (existing)
	let tmp = (NSTemporaryDirectory() as NSString).appendingPathComponent("lodo-menubar-test-\(getpid()).json")
	defer { try? FileManager.default.removeItem(atPath: tmp) }
	let json = """
	{"version":1,"domains":[
	  {"name":"blog.lodo","address":"127.0.0.1","enabled":true,"worktree":true},
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

	// 1b. Unsupported format version is ignored (existing)
	let badVer = """
	{"version":2,"domains":[{"name":"web.blog.lodo","address":"127.0.0.1","port":3000,"enabled":true}]}
	"""
	try! badVer.write(toFile: tmp, atomically: true, encoding: .utf8)
	check(loadCandidates(from: tmp).isEmpty, "unsupported version should return no candidates")

	// 2. Grouping (existing)
	let groups = groupRunning(candidates)
	check(groups.count == 2 && groups[0].project == "blog.lodo" && groups[1].project == "shop.custom", "grouping")

	// 3. Live TCP listener check & --dump format (existing + dump verification)
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

	let liveJSON = """
	{"version":1,"domains":[
	  {"name":"up.blog.lodo","address":"127.0.0.1","port":\(openPort),"enabled":true,"https":false},
	  {"name":"down.blog.lodo","address":"127.0.0.1","port":1,"enabled":true,"https":true}
	]}
	"""
	try! liveJSON.write(toFile: tmp, atomically: true, encoding: .utf8)
	let wantDump = "blog.lodo · 127.0.0.1\n● up.blog.lodo  (http :\(openPort))\n"
	check(dumpMenu(domainsPath: tmp) == wantDump, "dumpMenu live output mismatch")

	// 4. LsReport JSON decoding & system setup detection
	let reportJSON = """
	{
	  "setup_done": true,
	  "caddy_ready": true,
	  "next_free": "127.0.1.3",
	  "log_path": "/Users/tester/.config/lodo/dnsmasq.log",
	  "services": [
	    {"name": "dnsmasq", "state": "on", "detail": "", "fix": ""},
	    {"name": "caddy", "state": "off", "detail": "turned off", "fix": ""},
	    {"name": "menubar", "state": "error", "detail": "not loaded", "fix": "lodo menubar"},
	    {"name": "loopback", "state": "on", "detail": "", "fix": ""},
	    {"name": "resolvers", "state": "on", "detail": "", "fix": ""}
	  ],
	  "domains": [
	    {
	      "name": "blog.lodo",
	      "project": "blog.lodo",
	      "address": "127.0.1.1",
	      "port": 0,
	      "enabled": true,
	      "https": false,
	      "compose": "/Users/tester/blog/compose.yml",
	      "root": "/Users/tester/blog",
	      "status": "ok",
	      "https_ok": false,
	      "detail": ""
	    },
	    {
	      "name": "web.blog.lodo",
	      "project": "blog.lodo",
	      "address": "127.0.1.1",
	      "port": 3000,
	      "enabled": true,
	      "https": true,
	      "compose": "",
	      "root": "",
	      "status": "ok",
	      "https_ok": true,
	      "detail": ""
	    },
	    {
	      "name": "api.blog.lodo",
	      "project": "blog.lodo",
	      "address": "127.0.1.1",
	      "port": 4000,
	      "enabled": true,
	      "https": false,
	      "compose": "",
	      "root": "",
	      "status": "down",
	      "https_ok": false,
	      "detail": "nothing listens on 127.0.1.1:4000"
	    },
	    {
	      "name": "admin.blog.lodo",
	      "project": "blog.lodo",
	      "address": "127.0.1.1",
	      "port": 4001,
	      "enabled": false,
	      "https": false,
	      "compose": "",
	      "root": "",
	      "status": "off",
	      "https_ok": false,
	      "detail": ""
	    },
	    {
	      "name": "shop.custom",
	      "project": "shop.custom",
	      "address": "127.0.1.2",
	      "port": 5173,
	      "enabled": true,
	      "https": true,
	      "compose": "",
	      "root": "",
	      "status": "error",
	      "https_ok": false,
	      "detail": "https: Caddy's certificate isn't trusted: run lodo setup"
	    }
	  ]
	}
	"""
	let report = try! JSONDecoder().decode(LsReport.self, from: Data(reportJSON.utf8))
	check(report.setupDone && report.caddyReady, "LsReport booleans")
	check(report.nextFree == "127.0.1.3", "LsReport nextFree")
	check(report.logPath == "/Users/tester/.config/lodo/dnsmasq.log", "LsReport logPath")
	check(report.services.count == 5, "LsReport services count")
	check(report.domains.count == 5, "LsReport domains count")
	check(!needsSystemSetup(report), "menubar error alone should not flag system setup needed")

	let brokenReport = LsReport(
		setupDone: true,
		caddyReady: false,
		nextFree: nil,
		logPath: report.logPath,
		services: [LsService(name: "resolvers", state: "error", detail: "missing", fix: "lodo setup")],
		domains: []
	)
	check(needsSystemSetup(brokenReport), "resolvers error should flag system setup needed")
	let unsetupReport = LsReport(
		setupDone: false,
		caddyReady: false,
		nextFree: nil,
		logPath: report.logPath,
		services: [],
		domains: []
	)
	check(needsSystemSetup(unsetupReport), "!setupDone should flag system setup needed")

	// 5. Domain grouping, formatting, subdomain & parent lookup
	let lsGroups = groupLsDomains(report.domains)
	check(lsGroups.count == 2, "expected 2 LsProjectGroups, got \(lsGroups.count)")
	check(lsGroups[0].project == "blog.lodo" && lsGroups[0].address == "127.0.1.1", "first group header")
	check(lsGroups[0].domains.count == 4, "blog.lodo should contain all 4 configured domains")
	check(lsGroups[1].project == "shop.custom" && lsGroups[1].address == "127.0.1.2", "second group header")

	check(formatLsDomainRow(report.domains[0]) == "● blog.lodo  (dns)", "dns ok row")
	check(formatLsDomainRow(report.domains[1]) == "● web.blog.lodo  (https :3000)", "https ok row")
	check(formatLsDomainRow(report.domains[2]) == "○ api.blog.lodo  (http :4000, down)", "http down row")
	check(formatLsDomainRow(report.domains[3]) == "○ admin.blog.lodo  (http :4001, off)", "http off row")
	check(formatLsDomainRow(report.domains[4]) == "! shop.custom  (https :5173)", "https error row")

	let subs = subdomainsUnder("blog.lodo", in: report.domains)
	check(subs.map(\.name) == ["web.blog.lodo", "api.blog.lodo", "admin.blog.lodo"], "subdomainsUnder")
	check(parentDomain(for: "v1.api.blog.lodo", in: report.domains)?.name == "api.blog.lodo", "longest parent match")
	check(computeNextFreeAddress(in: report.domains) == "127.0.1.3", "computeNextFreeAddress")

	// 6. Name resolution & compose tilde expansion
	check(resolveDomainName("flowy") == "flowy.lodo", "bare label resolves to .lodo")
	check(resolveDomainName("api.shop.lodo") == "api.shop.lodo", "dotted domain preserved")
	check(resolveDomainName("shop.custom") == "shop.custom", "custom TLD preserved")
	check(resolveDomainName("api", parent: "shop.lodo") == "api.shop.lodo", "subdomain label appended")
	check(resolveDomainName("api.shop.lodo", parent: "shop.lodo") == "api.shop.lodo", "subdomain suffix not duplicated")
	check(resolveDomainName("", parent: "shop.lodo") == "", "empty subdomain stays empty")
	check(expandComposePath("~/app/compose.yml", home: "/Users/alice") == "/Users/alice/app/compose.yml", "tilde expand")

	// 7. Binary resolution order
	check(
		resolveLodoExecutable(
			env: ["LODO_BIN": "/custom/bin/lodo"],
			home: "/Users/alice",
			isExecutable: { _ in true }
		) == LodoExecutable(executable: "/custom/bin/lodo", prefixArgs: []),
		"LODO_BIN takes top priority"
	)
	check(
		resolveLodoExecutable(
			env: ["LODO_BIN": "/deleted/worktree/lodo"],
			home: "/Users/alice",
			isExecutable: { $0 == "/Users/alice/.cargo/bin/lodo" }
		) == LodoExecutable(executable: "/Users/alice/.cargo/bin/lodo", prefixArgs: []),
		"missing LODO_BIN falls back to installed binary"
	)
	check(
		resolveLodoExecutable(
			env: ["LODO_BIN": ""],
			home: "/Users/alice",
			isExecutable: { $0 == "/opt/homebrew/bin/lodo" }
		) == LodoExecutable(executable: "/opt/homebrew/bin/lodo", prefixArgs: []),
		"homebrew bin is second priority"
	)
	check(
		resolveLodoExecutable(
			env: [:],
			home: "/Users/alice",
			isExecutable: { $0 == "/Users/alice/.cargo/bin/lodo" }
		) == LodoExecutable(executable: "/Users/alice/.cargo/bin/lodo", prefixArgs: []),
		"cargo bin is third priority"
	)
	let fallbackExec = resolveLodoExecutable(env: [:], home: "/Users/alice", isExecutable: { _ in false })
	check(
		fallbackExec == LodoExecutable(executable: "/usr/bin/env", prefixArgs: ["lodo"]) &&
		fallbackExec.fullArguments(for: ["ls", "--json"]) == ["lodo", "ls", "--json"],
		"/usr/bin/env lodo fallback"
	)

	// 8. CLI command argument construction
	check(LodoCommand.lsJSON() == ["ls", "--json"], "lsJSON args")
	check(LodoCommand.service(name: "dnsmasq", on: true) == ["service", "dnsmasq", "on"], "service on args")
	check(LodoCommand.service(name: "caddy", on: false) == ["service", "caddy", "off"], "service off args")
	check(LodoCommand.setEnabled(name: "blog.lodo", enabled: true) == ["set", "blog.lodo", "--on"], "set --on")
	check(LodoCommand.setEnabled(name: "blog.lodo", enabled: false) == ["set", "blog.lodo", "--off"], "set --off")
	check(LodoCommand.setHTTPS(name: "web.blog.lodo", https: true) == ["set", "web.blog.lodo", "--https"], "set --https")
	check(LodoCommand.setHTTPS(name: "web.blog.lodo", https: false) == ["set", "web.blog.lodo", "--no-https"], "set --no-https")
	check(
		LodoCommand.addDomain(name: "flowy.lodo", address: "127.0.1.1", port: "", https: true, compose: "") ==
		["add", "flowy.lodo", "--address", "127.0.1.1"],
		"add minimal dns-only args"
	)
	check(
		LodoCommand.addDomain(
			name: "web.flowy.lodo",
			address: "127.0.1.1",
			port: "3000",
			https: true,
			compose: "~/flowy/compose.yml",
			home: "/Users/alice"
		) == [
			"add", "web.flowy.lodo",
			"--address", "127.0.1.1",
			"--port", "3000",
			"--https",
			"--compose", "/Users/alice/flowy/compose.yml"
		],
		"add full domain args"
	)
	check(
		LodoCommand.editDomain(
			originalName: "web.flowy.lodo",
			newName: "app.flowy.lodo",
			address: "127.0.1.2",
			port: "8080",
			enabled: true,
			https: true,
			compose: "/Users/alice/flowy/compose.yaml",
			home: "/Users/alice"
		) == [
			"set", "web.flowy.lodo",
			"--name", "app.flowy.lodo",
			"--address", "127.0.1.2",
			"--port", "8080",
			"--on",
			"--https",
			"--compose", "/Users/alice/flowy/compose.yaml"
		],
		"edit full domain args"
	)
	check(
		LodoCommand.editDomain(
			originalName: "web.flowy.lodo",
			newName: "web.flowy.lodo",
			address: "127.0.1.2",
			port: "",
			enabled: false,
			https: true,
			compose: "",
			home: "/Users/alice"
		) == [
			"set", "web.flowy.lodo",
			"--name", "web.flowy.lodo",
			"--address", "127.0.1.2",
			"--port", "none",
			"--off",
			"--no-https",
			"--compose", "none"
		],
		"edit clear port & compose args"
	)
	check(LodoCommand.removeDomain(name: "blog.lodo") == ["rm", "blog.lodo"], "rm args")
	check(LodoCommand.prune() == ["prune"], "prune args")

	// 9. NSMenuItem representedObject enum round-trip
	let testItem = NSMenuItem(title: "Test", action: nil, keyEquivalent: "")
	testItem.representedObject = MenuActionPayload.setDomainHTTPS(name: "web.blog.lodo", https: true)
	if case .setDomainHTTPS(let n, let h)? = testItem.representedObject as? MenuActionPayload {
		check(n == "web.blog.lodo" && h == true, "MenuActionPayload round-trip")
	} else {
		check(false, "MenuActionPayload failed to round-trip through representedObject")
	}

	// 10. DomainEditorModel prefill & state transitions
	let editorModel = DomainEditorModel()
	editorModel.reset(mode: .add, domains: report.domains, nextFree: report.nextFree)
	check(editorModel.addressInput == "127.0.1.3", "add mode starts with nextFree")
	editorModel.userUpdatedName("worker.shop.custom")
	check(editorModel.addressInput == "127.0.1.2", "typing subdomain of shop.custom prefills parent address")
	editorModel.userUpdatedName("fresh")
	check(editorModel.resolvedName == "fresh.lodo" && editorModel.addressInput == "127.0.1.3", "non-subdomain reverts to nextFree")
	editorModel.userUpdatedAddress("127.0.0.1")
	editorModel.userUpdatedName("worker.shop.custom")
	check(editorModel.addressInput == "127.0.0.1", "manual address edit stops auto-prefill")
	editorModel.useNextFree()
	check(editorModel.addressInput == "127.0.1.3", "useNextFree sets nextFree")

	editorModel.reset(
		mode: .addSubdomain(parent: "shop.custom", parentAddress: "127.0.1.2"),
		domains: report.domains,
		nextFree: report.nextFree
	)
	editorModel.userUpdatedName("api")
	check(editorModel.resolvedName == "api.shop.custom" && editorModel.addressInput == "127.0.1.2", "subdomain mode")

	editorModel.reset(mode: .edit(domain: report.domains[1]), domains: report.domains, nextFree: report.nextFree)
	check(
		editorModel.isEditing &&
		editorModel.nameInput == "web.blog.lodo" &&
		editorModel.portInput == "3000" &&
		editorModel.https &&
		editorModel.enabled,
		"edit mode prefill"
	)

	return 0
}

// MARK: - Entry Point

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
installMainMenu()
let controller = MenuController(domainsPath: domainsPath)
withExtendedLifetime(controller) {
	app.run()
}
