import XCTest
@testable import RedashWireCore

final class ConnectionStringsTests: XCTestCase {
    private func profile(postgres: String = "", mysql: String = "") -> Profile {
        Profile(
            name: "prod", redashURL: "https://redash.example.com", apiKeySet: true,
            postgresListenAddr: postgres, mysqlListenAddr: mysql,
            username: "redash-wire", password: "secret", defaultCredentials: true,
            pollInterval: "500ms", pollTimeout: "120s", valid: true, error: ""
        )
    }

    func testAWildcardBindBecomesLoopbackWhicheverFamilyItIs() {
        // A wildcard is not something a client can dial. The IPv6 one arrives
        // bracketed, which used to reach the command line as `-h [::]`.
        for addr in [":15432", "0.0.0.0:15432", "[::]:15432"] {
            XCTAssertEqual(ConnectionStrings.psql(profile: profile(postgres: addr), database: nil),
                           "psql -h 127.0.0.1 -p 15432 -U redash-wire", addr)
        }
    }

    func testAnIPv6HostIsBareForTheShellAndBracketedInTheURI() {
        let p = profile(postgres: "[::1]:15432", mysql: "[fe80::1%en0]:13306")

        XCTAssertEqual(ConnectionStrings.psql(profile: p, database: "Warehouse"),
                       "psql -h ::1 -p 15432 -U redash-wire -d Warehouse", "psql rejects brackets")
        XCTAssertEqual(ConnectionStrings.postgresURI(profile: p, database: "Warehouse"),
                       "postgresql://redash-wire:secret@[::1]:15432/Warehouse")

        XCTAssertEqual(ConnectionStrings.mysql(profile: p, database: nil),
                       "mysql -h fe80::1%en0 -P 13306 -u redash-wire -p")
        XCTAssertEqual(ConnectionStrings.mysqlURI(profile: p, database: nil),
                       "mysql://redash-wire:secret@[fe80::1%25en0]:13306", "a zone's % is not a URL escape")
    }

    func testAnIPv4HostPassesThroughUnchanged() {
        XCTAssertEqual(ConnectionStrings.postgresURI(profile: profile(postgres: "127.0.0.1:15432"), database: nil),
                       "postgresql://redash-wire:secret@127.0.0.1:15432")
    }

    func testAnAddressThatCannotBeSplitYieldsNoCommand() {
        // Nothing to offer is better than a command pointed at the wrong port.
        XCTAssertNil(ConnectionStrings.hostAndPort(from: "::1:15432"), "unbracketed IPv6 is ambiguous, as in Go")
        XCTAssertNil(ConnectionStrings.hostAndPort(from: "localhost"))
        XCTAssertNil(ConnectionStrings.hostAndPort(from: "localhost:"))
        XCTAssertNil(ConnectionStrings.psql(profile: profile(postgres: ""), database: nil), "a disabled listener")
    }
}
