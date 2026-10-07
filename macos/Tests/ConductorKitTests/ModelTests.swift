import XCTest
@testable import ConductorKit

final class ModelTests: XCTestCase {
    // MARK: - onboarding

    func testOnboardingOrder() {
        var f = OnboardingFacts()
        XCTAssertEqual(OnboardingFlow.next(f), .services)
        f.servicesRunning = true
        f.localStatus = LocalStatus(securityMode: "local", localLoginAvailable: true)
        XCTAssertEqual(OnboardingFlow.next(f), .signIn)
        f.signedIn = true
        XCTAssertEqual(OnboardingFlow.next(f), .repository)
        f.hasProject = true
        XCTAssertEqual(OnboardingFlow.next(f), .tools)
        f.toolsSkipped = true
        XCTAssertEqual(OnboardingFlow.next(f), .github)
        f.githubConfigured = true
        XCTAssertEqual(OnboardingFlow.next(f), .done)
    }

    func testAFreshDatabaseGoesToTheRepositoryBeforeSigningIn() {
        var f = OnboardingFacts()
        f.servicesRunning = true
        f.localStatus = LocalStatus(securityMode: "local", modeSource: "default", localLoginAvailable: false,
                                    reason: "no machine owner is set; run `conductord bootstrap` on this machine")
        XCTAssertTrue(f.waitingForOwner)
        XCTAssertEqual(OnboardingFlow.next(f), .repository)
        f.hasProject = true
        XCTAssertEqual(OnboardingFlow.next(f), .signIn)
    }

    func testSignInDecisions() {
        XCTAssertEqual(OnboardingFlow.signIn(LocalStatus(securityMode: "local", localLoginAvailable: true)), .automatic)
        XCTAssertEqual(OnboardingFlow.signIn(LocalStatus(securityMode: "local", localLoginAvailable: false, reason: "no machine owner is set; run x")), .needsOwner)
        XCTAssertEqual(OnboardingFlow.signIn(LocalStatus(securityMode: "enhanced", localLoginAvailable: false, reason: "enhanced")), .tokenRequired("enhanced"))
        XCTAssertEqual(OnboardingFlow.signIn(LocalStatus(securityMode: "local", localLoginAvailable: false, reason: "proxy")), .unavailable("proxy"))
    }

    func testLocalStatusDecodes() throws {
        let s = try JSONDecoder().decode(LocalStatus.self, from: Data(#"{"security_mode":"local","mode_source":"default","local_login_available":true,"reason":""}"#.utf8))
        XCTAssertTrue(s.localLoginAvailable)
        let session = try JSONDecoder().decode(LocalSession.self, from: Data(#"{"token":"cdt_x","handle":"ada","principal_id":"p1","expires_at":"2026-11-06T10:00:00Z"}"#.utf8))
        XCTAssertEqual(session.token, "cdt_x")
        XCTAssertEqual(session.handle, "ada")
    }

    // MARK: - menu bar

    func testStatusDot() throws {
        let json = """
        {"project":"conductor","counts":{"in_progress":1},"active":[{"id":"t1","ref":"T-1","status":"in_progress","owner":"ada","title":"x","project_id":"p","visibility":"team_summary","priority":0,"risk_level":"low","scopes":[],"fencing_epoch":1,"attempts_count":0,"updated_at":"2026-10-07T10:00:00Z","created_at":"2026-10-07T10:00:00Z"}],
         "ready":[],
         "conflicts":[{"id":"c1","kind":"write_write","severity":"high","suggestion":"suggest_join","state":"open","weight":0.9,"other":{"task_id":"t2","task_ref":"T-2","owner":"bob"},"mine":{"task_id":"t1","task_ref":"T-1"},"detected_at":"2026-10-07T10:00:00Z"},
                      {"id":"c2","kind":"read_write","severity":"low","suggestion":"suggest_wait","state":"resolved","weight":0.1,"other":{"task_id":"t3","task_ref":"T-3"},"mine":{"task_id":"t1","task_ref":"T-1"},"detected_at":"2026-10-07T10:00:00Z"}],
         "presence":[{"session_id":"s1","principal":"ada","principal_id":"u1","kind":"human","harness":"claude","state":"active","scopes":[],"started_at":"2026-10-07T09:00:00Z","last_heartbeat":"2026-10-07T10:00:00Z"},
                     {"session_id":"s2","principal":"bob","principal_id":"u2","kind":"human","harness":"codex","state":"active","scopes":null,"started_at":"2026-10-07T09:00:00Z","last_heartbeat":"2026-10-07T10:00:00Z"}]}
        """
        let status = try JSONDecoder().decode(StatusSummary.self, from: Data(json.utf8))
        XCTAssertEqual(status.openConflicts.map(\.id), ["c1"])
        XCTAssertEqual(status.openConflicts.first?.suggestionLabel, "join")
        XCTAssertEqual(status.sessions(of: "ada").map(\.sessionID), ["s1"])
        XCTAssertEqual(StatusDot.compute(daemonUp: false, status: status, offers: []), .down)
        XCTAssertEqual(StatusDot.compute(daemonUp: true, status: status, offers: []), .attention)

        var calm = status
        calm.conflicts = []
        XCTAssertEqual(StatusDot.compute(daemonUp: true, status: calm, offers: []), .clear)
        let offer = try JSONDecoder().decode(Assignment.self, from: Data(#"{"id":"a1","task_id":"t9","task_ref":"T-9","project_id":"p","session_id":"s1","requirement":{},"state":"offered","created_at":"2026-10-07T10:00:00Z","expires_at":"2026-10-07T10:15:00Z"}"#.utf8))
        XCTAssertEqual(StatusDot.compute(daemonUp: true, status: calm, offers: [offer]), .attention)
        XCTAssertEqual(StatusDot.compute(daemonUp: true, status: nil, offers: []), .clear)
    }

    func testEventReactions() {
        let quota = DomainEvent(id: "e", projectID: "p", aggregateType: "quota", aggregateID: "a", type: "quota.exhausted",
                                payload: .object(["harness": .string("claude"), "kind": .string("five_hour")]), occurredAt: nil)
        XCTAssertEqual(EventReaction.classify(quota), .usageLimit(harness: "claude", detail: "A claude login hit its usage limit (five_hour window)."))
        XCTAssertEqual(EventReaction.classify(DomainEvent(type: "task.claimed")), .refresh)
        XCTAssertEqual(EventReaction.classify(DomainEvent(type: "conflict.detected")), .refresh)
        XCTAssertEqual(EventReaction.classify(DomainEvent(type: "budget.shared")), .ignore)
    }

    // MARK: - settings

    func testAppSettingsRoundTrip() {
        let store = MemorySettingsStore()
        var s = AppSettings.load(from: store)
        XCTAssertEqual(s, AppSettings())
        XCTAssertEqual(s.endpoint, "http://127.0.0.1:8080")
        s.daemonPort = 8091
        s.project = "conductor"
        s.remember(repository: "/a")
        s.remember(repository: "/b")
        s.remember(repository: "/a")
        s.save(to: store)
        let again = AppSettings.load(from: store)
        XCTAssertEqual(again, s)
        XCTAssertEqual(again.repositories, ["/a", "/b"])
        XCTAssertEqual(again.endpoint, "http://127.0.0.1:8091")
        var attached = again
        attached.attachEndpoint = " https://conductor.team "
        XCTAssertTrue(attached.isAttached)
        XCTAssertEqual(attached.endpoint, "https://conductor.team")
    }

    func testPortProbe() {
        XCTAssertEqual(PortProbe.firstFree(from: 8080, count: 5, isFree: { $0 == 8082 }), 8082)
        XCTAssertNil(PortProbe.firstFree(from: 8080, count: 2, isFree: { _ in false }))
        XCTAssertFalse(PortProbe.isFree(0))
        XCTAssertFalse(PortProbe.isFree(70000))
    }

    // MARK: - API client

    func testRequestsAreLocalAndCarryNoOrigin() throws {
        let api = APIClient(endpoint: URL(string: "http://127.0.0.1:8080/")!)
        let req = api.request("POST", "/v1/local/session", body: Data(#"{"client":"mac-app"}"#.utf8))
        XCTAssertEqual(req.url?.absoluteString, "http://127.0.0.1:8080/v1/local/session")
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Content-Type"), "application/json")
        XCTAssertNil(req.value(forHTTPHeaderField: "Origin"))
        XCTAssertNil(req.value(forHTTPHeaderField: "Authorization"))
        let authed = api.withToken("cdt_t").request("GET", "/v1/projects/\(APIClient.segment("my proj/x"))/status")
        XCTAssertEqual(authed.url?.absoluteString, "http://127.0.0.1:8080/v1/projects/my%20proj%2Fx/status")
        XCTAssertEqual(authed.value(forHTTPHeaderField: "Authorization"), "Bearer cdt_t")
        XCTAssertEqual(api.eventStreamURL(project: "conductor").absoluteString, "http://127.0.0.1:8080/v1/projects/conductor/events/stream")
    }

    func testResponseDecoding() throws {
        let ok: LocalStatus = try APIClient.decodeResponse(data: Data(#"{"security_mode":"enhanced","local_login_available":false}"#.utf8), status: 200, as: LocalStatus.self)
        XCTAssertEqual(ok.securityMode, "enhanced")
        XCTAssertThrowsError(try APIClient.decodeResponse(data: Data(#"{"error":"this server is in enhanced security mode","code":"local_login_disabled"}"#.utf8), status: 403, as: LocalSession.self)) { error in
            XCTAssertEqual(error as? APIError, .server(status: 403, code: "local_login_disabled", message: "this server is in enhanced security mode"))
        }
        XCTAssertThrowsError(try APIClient.decodeResponse(data: Data("upstream down".utf8), status: 502, as: LocalSession.self)) { error in
            XCTAssertEqual(error as? APIError, .server(status: 502, code: nil, message: "upstream down"))
        }
        XCTAssertTrue(APIError.server(status: 401, code: nil, message: "").isUnauthenticated)
    }

    func testWhoAmIAndInviteResult() throws {
        let who = try JSONDecoder().decode(WhoAmI.self, from: Data(#"{"principal":{"id":"u1","handle":"ada","kind":"human"},"projects":[{"id":"p1","slug":"conductor","role":"org_admin"}]}"#.utf8))
        XCTAssertEqual(who.principal.handle, "ada")
        XCTAssertEqual(who.projects.map(\.slug), ["conductor"])
        let invite = try JSONDecoder().decode(InviteResult.self, from: Data(#"{"handle":"rachel","role":"contributor","token":"cdt_r","expires_at":"2026-10-14T10:00:00Z","created_principal":true,"existing_principal":false,"note":""}"#.utf8))
        XCTAssertEqual(invite.token, "cdt_r")
    }
}

extension DomainEvent {
    init(type: String) {
        self.init(id: nil, projectID: nil, aggregateType: nil, aggregateID: nil, type: type, payload: nil, occurredAt: nil)
    }
}
