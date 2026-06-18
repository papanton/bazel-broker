import Foundation

/// Runs a helper process to completion, capturing combined stdout/stderr. Used to drive
/// the bundled shell scripts (install.sh / setup.sh) and binaries. Allowed because the
/// app is non-sandboxed by design. Reading happens on a background queue BEFORE
/// `waitUntilExit()` so a chatty child can never deadlock on a full pipe buffer.
///
/// The bundled scripts are `#!/usr/bin/env bash` and shell out to mktemp/sed/awk, so the
/// child's `PATH` is always sanitized here (a GUI app's environment can lack a usable one)
/// — the single home for that concern, shared by every caller.
enum ProcessRunner {
    static func run(_ launchPath: String, _ args: [String],
                    env extraEnv: [String: String] = [:]) -> DaemonActionResult {
        var env = ProcessInfo.processInfo.environment
        for (k, v) in extraEnv { env[k] = v }
        env["PATH"] = sanitizedPATH(env["PATH"])

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: launchPath)
        proc.arguments = args
        proc.environment = env

        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = pipe

        // Drain the pipe concurrently so the child never blocks writing while we wait.
        var collected = Data()
        let lock = NSLock()
        let group = DispatchGroup()
        group.enter()
        DispatchQueue.global().async {
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            lock.lock(); collected = data; lock.unlock()
            group.leave()
        }

        do { try proc.run() } catch {
            return DaemonActionResult(ok: false, message: "launch failed: \(error.localizedDescription)")
        }
        proc.waitUntilExit()
        group.wait()

        lock.lock(); let data = collected; lock.unlock()
        let text = String(data: data, encoding: .utf8)?
            .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return DaemonActionResult(ok: proc.terminationStatus == 0, message: text)
    }

    /// Prepend the standard system tool dirs so `/usr/bin/env bash` and the tools the
    /// scripts call (mktemp, sed, awk, …) resolve even from a sparse GUI-app environment.
    static func sanitizedPATH(_ existing: String?) -> String {
        let system = "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin"
        guard let existing, !existing.isEmpty else { return system }
        return existing.contains("/usr/bin") ? existing : "\(system):\(existing)"
    }
}
