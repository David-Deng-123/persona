import XCTest

/// The open plan's trust offer and the exact Start request against the loopback mock hub (#17853).
/// Run once with the default start_authorization fixture and once with MOCK_PLAN_START_REASON=owner_step.
final class PlanStartUITests: XCTestCase {
    @MainActor private var app: XCUIApplication!

    override func setUpWithError() throws { continueAfterFailure = false }

    private func hub() throws -> String {
        guard let hub = ProcessInfo.processInfo.environment["PERSONA_MOCK_HUB"],
              let url = URL(string: hub), url.scheme == "http", url.host == "127.0.0.1" else {
            throw XCTSkip("needs scripts/mock/mock_hub.py (PERSONA_MOCK_HUB)")
        }
        return hub
    }

    private func object(_ hub: String, _ path: String) throws -> [String: Any] {
        let data = try Data(contentsOf: URL(string: hub + path)!)
        return try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    private func wait(_ check: () throws -> Bool) -> Bool {
        let deadline = Date.now.addingTimeInterval(15)
        while Date.now < deadline {
            if (try? check()) == true { return true }
            usleep(200_000)
        }
        return false
    }

    @MainActor private func reveal(_ element: XCUIElement) -> Bool {
        for _ in 0..<16 {
            let frame = element.frame
            let y = frame.midY
            if element.isHittable && y > app.frame.height * 0.25 && y < app.frame.height * 0.65 { return true }
            let below = frame == .zero || y > app.frame.midY
            let from = app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: below ? 0.70 : 0.30))
            let to = app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: below ? 0.65 : 0.35))
            from.press(forDuration: 0.1, thenDragTo: to)
        }
        let y = element.frame.midY
        return element.isHittable && y > app.frame.height * 0.25 && y < app.frame.height * 0.65
    }

    @MainActor private func shot(_ name: String) {
        let screenshot = app.screenshot()
        let attachment = XCTAttachment(screenshot: screenshot)
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
        if let dir = ProcessInfo.processInfo.environment["PERSONA_SHOTS"] {
            try? screenshot.pngRepresentation.write(to: URL(fileURLWithPath: dir).appendingPathComponent(name + ".png"))
        }
    }

    @MainActor func testTrustOfferAndStartRequestFollowTheHubProjection() throws {
        let hub = try hub()
        let body = try object(hub, "/api/v1/task-plans/plan_mock_3")
        let plan = try XCTUnwrap(body["plan"] as? [String: Any])
        let start = plan["start"] as? [String: Any]
        let offered: Bool
        if let start {
            let indexes = start["remember"] as? [Any] ?? []
            offered = start["reason"] as? String == "start_authorization" && !indexes.isEmpty
        } else {
            offered = true  // an older hub keeps the legacy offer
        }

        for (look, size) in [("light", "standard"), ("dark", "standard"), ("light", "largest"), ("dark", "largest")] {
            app = XCUIApplication()
            let pair = "persona://pair?url=" + hub.addingPercentEncoding(withAllowedCharacters: .alphanumerics)! + "&token=mock"
            app.launchArguments = ["-pairURL", pair, "-openChatsFresh", "YES", "-openAssistant",
                                   "server/b2e4d6f8-0000-4000-8000-000000000001", "-app.tab", "assistant", "-ui.scheme", look]
            if size == "largest" {
                app.launchArguments += ["-UIPreferredContentSizeCategoryName", "UICTContentSizeCategoryAccessibilityXXXL"]
            }
            app.launchPinned()
            HomeScreen.dismissIconNote()
            let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
            let allow = springboard.buttons.matching(NSPredicate(format: "label IN %@", ["Allow", "允许"])).firstMatch
            if allow.waitForExistence(timeout: 3) { allow.tap() }

            let title = app.descendants(matching: .any)["处理 PR #42 的 review 意见"]
            for _ in 0..<12 where !title.isHittable { app.swipeDown() } // large text can leave the plan outside the lazy list
            XCTAssertTrue(title.waitForExistence(timeout: 10), "the open plan never rendered")
            XCTAssertTrue(title.isHittable, "the open plan never entered the viewport")
            HomeScreen.dismissIconNote()
            let trust = app.buttons["这些目录及子目录以后直接开"]
            let startButton = app.buttons.matching(NSPredicate(format: "label == %@", "开始")).firstMatch
            XCTAssertTrue(startButton.waitForExistence(timeout: 20), "withholding trust must not withhold Start")
            if offered {
                XCTAssertTrue(trust.waitForExistence(timeout: 10), "the server's uncovered task index was not offered")
                XCTAssertTrue(reveal(trust), "the offered trust control never entered the viewport")
            } else {
                XCTAssertFalse(trust.waitForExistence(timeout: 2), "a hard gate still offered an ineffective trust rule")
            }
            if look == "dark" && size == "largest" && offered {
                trust.tap() // select it before bringing Start into view; both need not fit at the largest text size
            }
            XCTAssertTrue(reveal(startButton), "the plan's Start must be reachable before capture")
            RunLoop.current.run(until: Date().addingTimeInterval(0.8)) // capture the settled scroll position, not an animation frame
            shot((offered ? "plan-start-offered-" : "plan-start-withheld-") + look + "-" + size)

            if look != "dark" || size != "largest" {
                app.terminate()  // keep the same open plan for the next appearance
                continue
            }
            startButton.tap()
            XCTAssertTrue(wait {
                let log = try self.object(hub, "/mock/plan-starts")
                guard let requests = log["requests"] as? [[String: Any]], let last = requests.last else { return false }
                return last["remember"] as? Bool == offered
            }, "Start did not send remember=\(offered) for the current projection")
        }
    }
}
