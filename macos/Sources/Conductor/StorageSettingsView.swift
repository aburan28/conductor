import AppKit
import ConductorKit
import SwiftUI

/// Settings → Storage: the bucket that holds session backups and checkpoints and makes the
/// local Postgres durable, and how to sign in to it (docs/STORAGE.md). The CLI owns
/// storage.json; this pane reads it with `conductor storage show --json` and saves through
/// `conductor storage set`, keeping the secrets in the Keychain.
@MainActor
final class StoragePane: ObservableObject {
    @Published var form = StorageForm()
    @Published var show: StorageShow?
    @Published var loadProblem: String?
    @Published var profiles: [AWSProfile] = []
    @Published var awsCLI: URL?
    @Published var testResult: StorageTestResult?
    @Published var databaseStatus: DatabaseStatus?
    @Published var databaseStatusLoaded = false
    @Published var busy: String?
    @Published var problems: [String] = []
    @Published var saved = false
    @Published var secretStored = false
    @Published var sealStored = false
    @Published var endpointExpanded = false

    private let model: AppModel
    private var controller: StorageController {
        let model = self.model
        return StorageController(secrets: model.secrets) { args, stdin in
            try await model.runConductor(args, stdin: stdin)
        }
    }

    init(model: AppModel) {
        self.model = model
    }

    func load() async {
        busy = "Reading the storage settings…"
        defer { busy = nil }
        switch await controller.load() {
        case .success(let s):
            show = s
            form = StorageForm(s)
            loadProblem = nil
        case .failure(let e):
            loadProblem = e.description
        }
        endpointExpanded = form.usesCustomEndpoint
        refreshSecrets()
        awsCLI = AWSProfiles.locateCLI(environment: model.commandEnvironment)
        profiles = await controller.profiles(home: model.paths.home, environment: model.environment)
        await loadDatabaseStatus(local: true)
    }

    func refreshSecrets() {
        secretStored = controller.secretInKeychain(accessKeyID: form.accessKeyID)
        sealStored = controller.sealPassphraseSet
    }

    func save() async {
        problems = controller.problems(form, environment: model.environment)
        guard problems.isEmpty else { return }
        busy = "Saving…"
        defer { busy = nil }
        do {
            let s = try await controller.save(form, environment: model.environment)
            show = s
            // The secrets went to the Keychain; nothing of them stays in memory.
            form.newSecret = ""
            form.newSealPassphrase = ""
            saved = true
            refreshSecrets()
            busy = "Applying to the database…"
            await model.applyStorage(s)
            await loadDatabaseStatus(local: true)
        } catch let e as StorageCommandError {
            if case .invalid(let list) = e { problems = list } else { problems = [e.description] }
        } catch {
            problems = [String(describing: error)]
        }
    }

    func test() async {
        busy = "Testing the bucket…"
        defer { busy = nil }
        testResult = nil
        switch await controller.test() {
        case .success(let r): testResult = r
        case .failure(let e): problems = [e.description]
        }
    }

    func unset() async {
        busy = "Removing the storage settings…"
        defer { busy = nil }
        do {
            try await controller.unset()
            await load()
            if let s = show { await model.applyStorage(s) }
        } catch {
            problems = [String(describing: error)]
        }
    }

    func loadDatabaseStatus(local: Bool) async {
        databaseStatus = await controller.databaseStatus(local: local)
        databaseStatusLoaded = true
    }

    /// `aws sso login --profile NAME`: the AWS CLI opens the browser and waits for it.
    func ssoLogin(_ profile: String) async {
        guard let aws = awsCLI else { return }
        busy = "Waiting for the SSO sign-in in your browser…"
        defer { busy = nil }
        do {
            let r = try await ProcessRunner().run(CommandSpec(aws, AWSProfiles.ssoLoginArguments(profile: profile),
                                                              environment: model.commandEnvironment))
            if !r.succeeded { problems = [r.failureMessage("aws sso login")] }
        } catch {
            problems = [String(describing: error)]
        }
    }
}

struct StorageSettingsView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StorageSettingsForm(pane: StoragePaneHolder.shared.pane(for: model))
    }
}

/// Keeps one pane model for the life of the app, so switching tabs keeps what was typed.
@MainActor
final class StoragePaneHolder {
    static let shared = StoragePaneHolder()
    private var made: StoragePane?

    func pane(for model: AppModel) -> StoragePane {
        if let made { return made }
        let p = StoragePane(model: model)
        made = p
        return p
    }
}

@MainActor
struct StorageSettingsForm: View {
    @ObservedObject var pane: StoragePane
    @EnvironmentObject var model: AppModel

    var body: some View {
        Form {
            // What the app refused or failed to do with these settings, such as applying them
            // while a restore is on offer. Onboarding shows the same banner.
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let problem = pane.loadProblem {
                Text("The storage settings could not be read: \(problem)")
                    .foregroundStyle(.red).font(.callout).fixedSize(horizontal: false, vertical: true)
            }
            if pane.show?.overriddenByEnvironment == true {
                Label("CONDUCTOR_BACKUP_S3_* variables are set and override these settings.", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.orange)
            }
            if pane.show?.off == true {
                Label("CONDUCTOR_BACKUP=off: nothing is uploaded until it is unset.", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.orange)
            }

            Section {
                TextField("Bucket", text: $pane.form.bucket, prompt: Text("my-team-conductor"))
                TextField("Region", text: $pane.form.region, prompt: Text(pane.show?.effectiveRegion ?? "us-east-1"))
                TextField("Prefix", text: $pane.form.prefix, prompt: Text("conductor"))
                DisclosureGroup("S3-compatible endpoint", isExpanded: $pane.endpointExpanded) {
                    TextField("Endpoint", text: $pane.form.endpoint, prompt: Text("https://minio.example.com:9000"))
                    Toggle("Path-style addressing", isOn: $pane.form.pathStyle)
                        .help("https://host/bucket/key; most stores other than AWS need it")
                    Toggle("Allow plain HTTP", isOn: $pane.form.insecure)
                        .help("Only for an endpoint on your own network, such as a local MinIO")
                }
            } header: {
                Text("Bucket")
            } footer: {
                Text("Leave the endpoint empty for Amazon S3. MinIO, Cloudflare R2, Ceph and other S3-compatible stores take their URL.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section("Keep in the bucket") {
                Toggle("Session backups", isOn: $pane.form.sessions)
                    .help("conductor backup: a replaced Mac reopens its sessions")
                Toggle("Checkpoints", isOn: $pane.form.checkpoints)
                    .help("conductor checkpoint push: continue a conversation on another machine; always sealed")
                Toggle("The database", isOn: $pane.form.database)
                    .help("Archive every change of the local Postgres and take base backups, so another Mac can restore it")
            }

            SignInSection(pane: pane)

            if pane.form.database {
                DatabaseSection(pane: pane)
            }

            Section {
                HStack {
                    Button("Save") { Task { await pane.save() } }
                        .keyboardShortcut(.defaultAction)
                    Button("Test Connection") { Task { await pane.test() } }
                        .disabled(pane.show?.configured != true)
                        .help("Signs in and puts, gets, lists and deletes a probe object")
                    if let busy = pane.busy {
                        ProgressView().controlSize(.small)
                        Text(busy).font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Remove…") { Task { await pane.unset() } }
                        .disabled(pane.show?.configured != true)
                        .help("conductor storage unset: stop using the bucket; what is in it stays")
                }
                .disabled(pane.busy != nil)
                ForEach(pane.problems, id: \.self) { p in
                    Text(p).foregroundStyle(.red).font(.callout).textSelection(.enabled)
                }
                if let result = pane.testResult {
                    TestResultView(result: result)
                }
                if let description = pane.show?.authDescription, pane.show?.configured == true {
                    Text("Saved: \(description).").font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .formStyle(.grouped)
        .task { await pane.load() }
    }
}

@MainActor
private struct SignInSection: View {
    @ObservedObject var pane: StoragePane

    var body: some View {
        Section {
            Picker("Method", selection: $pane.form.method) {
                ForEach(StorageAuthMethod.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.radioGroup)

            switch pane.form.method {
            case .static:
                TextField("Access key ID", text: $pane.form.accessKeyID, prompt: Text("AKIA…"))
                    .onChange(of: pane.form.accessKeyID) { _ in pane.refreshSecrets() }
                SecureField("Secret access key", text: $pane.form.newSecret,
                            prompt: Text(pane.secretStored ? "Stored in the Keychain; type to replace" : "Secret access key"))
                Text("The secret is kept in your login Keychain (dev.conductor.s3), readable by this app and by the conductor command without a prompt. It is never written to a file.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            case .profile:
                Picker("Profile", selection: $pane.form.profile) {
                    Text("Default (AWS_PROFILE, then default)").tag("")
                    ForEach(pane.profiles) { p in
                        Text("\(p.name) — \(p.kindLabel)\(p.region.map { ", \($0)" } ?? "")").tag(p.name)
                    }
                    if !pane.form.profile.isEmpty && !pane.profiles.contains(where: { $0.name == pane.form.profile }) {
                        Text("\(pane.form.profile) (not found)").tag(pane.form.profile)
                    }
                }
                if let selected = pane.profiles.first(where: { $0.name == pane.form.profile }), selected.isSSO {
                    if pane.awsCLI != nil {
                        Button("Sign in with SSO") { Task { await pane.ssoLogin(selected.name) } }
                            .help("aws sso login --profile \(selected.name)")
                    } else {
                        Text("This profile signs in through IAM Identity Center, which needs the AWS CLI: install it (https://aws.amazon.com/cli/, or brew install awscli) and this button runs aws sso login for you. Until then, run aws sso login --profile \(selected.name) wherever the AWS CLI is.")
                            .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                Text("Profiles come from ~/.aws/config and ~/.aws/credentials (AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE when set). Static keys, credential_process, SSO and role_arn with source_profile all work; Conductor stores nothing secret.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            case .environment:
                Text("Whatever the machine provides, tried in this order: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN; a web identity token (AWS_WEB_IDENTITY_TOKEN_FILE with AWS_ROLE_ARN); ECS container credentials; the EC2 instance role through IMDSv2. Meant for servers and CI: a Mac's launch agents do not get secret variables.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        } header: {
            Text("Sign in")
        }
    }
}

@MainActor
private struct DatabaseSection: View {
    @ObservedObject var pane: StoragePane

    var body: some View {
        Section {
            Toggle("Archive every write-ahead-log segment", isOn: $pane.form.archiveWAL)
                .help("Postgres uploads each WAL segment as it fills, or at least every \(pane.form.archiveTimeoutSeconds) seconds")
            Stepper("Base backup every \(pane.form.baseBackupEveryHours) hour\(pane.form.baseBackupEveryHours == 1 ? "" : "s")",
                    value: $pane.form.baseBackupEveryHours, in: 1...168)
            Stepper("Keep \(pane.form.keepBaseBackups) base backup\(pane.form.keepBaseBackups == 1 ? "" : "s")",
                    value: $pane.form.keepBaseBackups, in: 1...60)
            Toggle("Seal (encrypt before upload)", isOn: $pane.form.seal)
            if pane.form.seal {
                SecureField("Seal passphrase", text: $pane.form.newSealPassphrase,
                            prompt: Text(pane.sealStored ? "Stored in the Keychain; type to replace" : "A passphrase only you know"))
                Text("Kept in your login Keychain (dev.conductor.seal). Another Mac restoring this database needs the bucket and this passphrase; lose it and the sealed backups cannot be opened.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            LabeledContent("Status") {
                Button("Refresh from the Bucket") { Task { await pane.loadDatabaseStatus(local: false) } }
                    .controlSize(.small)
            }
            if let status = pane.databaseStatus {
                ForEach(status.rows) { row in
                    LabeledContent(row.label) { Text(row.value).textSelection(.enabled) }
                }
                if let problem = status.problem {
                    Text(problem).foregroundStyle(.orange).font(.callout).fixedSize(horizontal: false, vertical: true)
                }
            } else if pane.databaseStatusLoaded {
                Text("Not available: this conductor command does not report the database's status yet.")
                    .font(.callout).foregroundStyle(.secondary)
            }
        } header: {
            Text("Database durability")
        } footer: {
            Text("Postgres stays on this Mac and fast; the bucket makes it durable. A lost Mac restores to within about a minute of where it stopped, on any machine that can reach the bucket.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}

private struct TestResultView: View {
    let result: StorageTestResult

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Label(result.ok ? "The bucket is reachable and writable." : "The test failed.",
                  systemImage: result.ok ? "checkmark.circle.fill" : "xmark.octagon.fill")
                .foregroundStyle(result.ok ? Color.green : Color.red)
            if !result.location.isEmpty || !result.credentials.isEmpty {
                Text([result.location, result.credentials].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary)
            }
            ForEach(result.steps) { step in
                HStack(alignment: .firstTextBaseline) {
                    Image(systemName: step.ok ? "checkmark" : "xmark")
                        .foregroundStyle(step.ok ? Color.green : Color.red)
                        .frame(width: 14)
                    Text(step.name).frame(width: 90, alignment: .leading)
                    Text("\(step.ms) ms").monospacedDigit().foregroundStyle(.secondary).frame(width: 70, alignment: .trailing)
                    if let error = step.error {
                        Text(error).foregroundStyle(.red).textSelection(.enabled)
                    }
                }
                .font(.callout)
            }
            if let error = result.error {
                Text(error).foregroundStyle(.red).font(.callout).textSelection(.enabled)
            }
        }
    }
}
