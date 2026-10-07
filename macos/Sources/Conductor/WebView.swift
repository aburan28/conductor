import AppKit
import SwiftUI
import WebKit

/// One dashboard web view. The dashboard is served by conductord on the same origin it
/// signs in against (`POST /v1/local/session`), so nothing is injected into the page.
@MainActor
final class Browser: NSObject, ObservableObject, WKNavigationDelegate, WKUIDelegate {
    let view: WKWebView
    @Published private(set) var failure: String?
    private var origin: URL?

    override init() {
        let configuration = WKWebViewConfiguration()
        // How the dashboard can tell it is inside the app rather than a browser tab.
        configuration.applicationNameForUserAgent = "ConductorApp/1"
        view = WKWebView(frame: .zero, configuration: configuration)
        super.init()
        view.navigationDelegate = self
        view.uiDelegate = self
        view.allowsBackForwardNavigationGestures = true
    }

    func show(_ url: URL) {
        origin = url
        failure = nil
        if view.url?.host != url.host || view.url?.port != url.port {
            view.load(URLRequest(url: url))
        }
    }

    func reload() {
        failure = nil
        if view.url == nil, let origin {
            view.load(URLRequest(url: origin))
        } else {
            view.reload()
        }
    }

    /// The dashboard's own pages stay here; a link elsewhere (GitHub, a pull request) goes to
    /// the person's browser, and only a link they clicked, to http, https or mail. A page
    /// must not hand `file:`, `smb:` or another app's scheme to the system unasked.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping @MainActor (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else {
            decisionHandler(.cancel)
            return
        }
        if isDashboard(url) {
            decisionHandler(.allow)
            return
        }
        if Self.mayOpenExternally(url, action: action) {
            NSWorkspace.shared.open(url)
        }
        decisionHandler(.cancel)
    }

    /// `target="_blank"`: this app has one dashboard per window, so it opens here or outside.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url {
            if isDashboard(url) {
                webView.load(URLRequest(url: url))
            } else if Self.mayOpenExternally(url, action: action) {
                NSWorkspace.shared.open(url)
            }
        }
        return nil
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        failure = error.localizedDescription
    }

    static func mayOpenExternally(_ url: URL, action: WKNavigationAction) -> Bool {
        guard action.navigationType == .linkActivated else { return false }
        return ["http", "https", "mailto"].contains(url.scheme?.lowercased() ?? "")
    }

    private func isDashboard(_ url: URL) -> Bool {
        guard let scheme = url.scheme?.lowercased() else { return false }
        if scheme == "about" || scheme == "blob" || scheme == "data" { return true }
        guard scheme == "http" || scheme == "https", let origin else { return false }
        let loopback: Set<String> = ["127.0.0.1", "localhost"]
        let host = url.host?.lowercased() ?? ""
        let originHost = origin.host?.lowercased() ?? ""
        let sameHost = host == originHost || (loopback.contains(host) && loopback.contains(originHost))
        return sameHost && url.port == origin.port
    }
}

struct WebView: NSViewRepresentable {
    let browser: Browser

    func makeNSView(context: Context) -> WKWebView { browser.view }
    func updateNSView(_ view: WKWebView, context: Context) {}
}
