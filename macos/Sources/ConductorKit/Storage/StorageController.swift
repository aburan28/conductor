import Foundation

/// What Settings → Storage does, without the view: load the settings, save them through
/// `conductor storage set`, keep the secrets in the Keychain, test the bucket, and read the
/// database's status.
///
/// Secrets: an access key's secret and the seal passphrase are written by this app to the
/// Keychain as generic passwords whose access list trusts this app and /usr/bin/security,
/// through which the CLI reads them without a prompt (docs/STORAGE.md). They are never
/// written to disk or to UserDefaults, and never put on a command line.
public struct StorageController: Sendable {
    public typealias Run = @Sendable (_ arguments: [String], _ stdin: Data?) async throws -> CommandResult

    public let secrets: SecretStore
    public let run: Run

    public init(secrets: SecretStore, run: @escaping Run) {
        self.secrets = secrets
        self.run = run
    }

    // MARK: - settings

    /// `conductor storage show --json`.
    public func load() async -> Result<StorageShow, StorageCommandError> {
        do {
            let r = try await run(ConductorCommands.storageShow, nil)
            guard r.succeeded else { return .failure(.command(r.failureMessage("conductor storage show"))) }
            return .success(try StorageShow.decode(r.stdout))
        } catch let e as StorageCommandError {
            return .failure(e)
        } catch {
            return .failure(.command(String(describing: error)))
        }
    }

    public func secretInKeychain(accessKeyID: String) -> Bool {
        let id = accessKeyID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !id.isEmpty else { return false }
        return (try? secrets.exists(service: KeychainNames.s3Service, account: id)) ?? false
    }

    public var sealPassphraseSet: Bool {
        (try? secrets.exists(service: KeychainNames.sealService, account: KeychainNames.sealAccount)) ?? false
    }

    /// What stops `form` from being saved.
    public func problems(_ form: StorageForm, environment: [String: String] = [:]) -> [String] {
        let sealFromEnvironment = !(environment["CONDUCTOR_CHECKPOINT_KEY"] ?? "").isEmpty
        return form.problems(secretInKeychain: secretInKeychain(accessKeyID: form.accessKeyID),
                             sealPassphraseSet: sealPassphraseSet || sealFromEnvironment)
    }

    /// Saves the pane: secrets to the Keychain first, then `conductor storage set` with every
    /// setting the pane shows. Returns the settings as the CLI now reports them.
    ///
    /// A new access-key secret is written by the app, with the access list the CLI needs, and
    /// `storage set` is told `--secret-from keychain`, which makes it read the item back
    /// through /usr/bin/security: proof, at Save, that the CLI can read it. If the app cannot
    /// write the item, the secret goes to `storage set --secret-from stdin` on standard input
    /// instead, and the CLI stores it in the Keychain itself (`--secret-store keychain`).
    public func save(_ form: StorageForm, environment: [String: String] = [:]) async throws -> StorageShow {
        let found = problems(form, environment: environment)
        guard found.isEmpty else { throw StorageCommandError.invalid(found) }

        if !form.newSealPassphrase.isEmpty {
            try setSealPassphrase(form.newSealPassphrase)
        }

        var source = StorageForm.SecretSource.keychain
        var stdin: Data?
        if form.method == .static, !form.trimmedSecret.isEmpty {
            do {
                try secrets.writeShared(form.trimmedSecret, service: KeychainNames.s3Service,
                                        account: form.trimmedAccessKeyID,
                                        label: "Conductor S3 secret (\(form.trimmedAccessKeyID))")
            } catch {
                source = .stdin
                stdin = form.secretStdin
            }
        }

        let result = try await run(form.setArguments(secret: source), stdin)
        guard result.succeeded else {
            throw StorageCommandError.command(result.failureMessage("conductor storage set"))
        }
        if let show = try? StorageShow.decode(result.stdout) { return show }
        return try await load().get()
    }

    /// The seal passphrase, in the Keychain item the CLI falls back to on macOS.
    public func setSealPassphrase(_ passphrase: String) throws {
        try secrets.writeShared(passphrase, service: KeychainNames.sealService, account: KeychainNames.sealAccount,
                                label: "Conductor seal passphrase")
    }

    /// `conductor storage unset`, which also removes the access key's Keychain item.
    public func unset() async throws {
        let r = try await run(ConductorCommands.storageUnset, nil)
        guard r.succeeded else { throw StorageCommandError.command(r.failureMessage("conductor storage unset")) }
    }

    // MARK: - test, profiles, database

    /// `conductor storage test --json`, decoded whatever its exit status.
    public func test() async -> Result<StorageTestResult, StorageCommandError> {
        do {
            return StorageTestResult.from(try await run(ConductorCommands.storageTest, nil))
        } catch {
            return .failure(.command(String(describing: error)))
        }
    }

    /// The person's AWS profiles, read from their files; where `conductor storage profiles
    /// --json` answers, its classification wins, so the pane and the CLI agree.
    public func profiles(home: URL, environment: [String: String]) async -> [AWSProfile] {
        var local = AWSProfiles.load(home: home, environment: environment)
        guard let r = try? await run(ConductorCommands.storageProfiles, nil), r.succeeded,
              let fromCLI = CLIProfile.decodeList(r.stdout) else { return local }
        for p in fromCLI {
            if let i = local.firstIndex(where: { $0.name == p.name }) {
                local[i].kind = p.kind
                if local[i].region == nil { local[i].region = p.region }
            } else {
                local.append(AWSProfile(name: p.name, kind: p.kind, region: p.region, isSSO: p.kind == "sso", sources: ["conductor"]))
            }
        }
        return local
    }

    /// `conductor db status --json`, or nil when it is not available (an older CLI, or one
    /// that failed), which the pane says in those words. `local` asks only what this machine
    /// recorded, without the network: what a frequent poll uses.
    public func databaseStatus(local: Bool = false) async -> DatabaseStatus? {
        guard let r = try? await run(local ? ConductorCommands.dbStatusLocal : ConductorCommands.dbStatus, nil) else { return nil }
        // Decoded whatever the status: a command that reports a problem may still say why.
        return DatabaseStatus.decode(r.stdout)
    }

    /// `conductor db backups --json`, or nil when it is not available.
    public func baseBackups() async -> BaseBackupList? {
        guard let r = try? await run(ConductorCommands.dbBackups, nil), r.succeeded else { return nil }
        return BaseBackupList.decode(r.stdout)
    }
}
