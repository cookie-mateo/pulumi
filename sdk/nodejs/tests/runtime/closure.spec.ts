// Copyright 2016-2017, Pulumi Corporation.  All rights reserved.

import * as assert from "assert";
import { runtime } from "../../index";

interface ClosureCase {
    title: string;            // a title banner for the test case.
    func: Function;           // the function whose body and closure to serialize.
    expect?: runtime.Closure; // if undefined, error expected; otherwise, the serialized shape.
}

// This group of tests ensure that we serialize closures properly.
describe("closure", () => {
    let cases: ClosureCase[] = [];

    // A few simple positive cases for functions/arrows (no captures).
    cases.push({
        title: "Empty function closure",
        func: function () { },
        expect: {
            code: "(function () { })",
            environment: {},
            runtime: "nodejs",
        },
    });
    cases.push({
        title: "Empty arrow closure",
        func: () => { },
        expect: {
            code: "(() => { })",
            environment: {},
            runtime: "nodejs",
        },
    });
    cases.push({
        title: "Empty function closure w/ args",
        func: function (x: any, y: any, z: any) { },
        expect: {
            code: "(function (x, y, z) { })",
            environment: {},
            runtime: "nodejs",
        },
    });
    cases.push({
        title: "Empty arrow closure w/ args",
        func: (x: any, y: any, z: any) => { },
        expect: {
            code: "((x, y, z) => { })",
            environment: {},
            runtime: "nodejs",
        },
    });

    // Ensure we reject function declarations.
    class C {
        public m(): void {}
    }
    cases.push({
        title: "Reject non-expression function objects",
        func: new C().m,
    });

    // Serialize captures.
    cases.push({
        title: "Doesn't serialize global captures",
        func: () => { console.log("Just a global object reference"); },
        expect: {
            code: `(() => { console.log("Just a global object reference"); })`,
            environment: {},
            runtime: "nodejs",
        },
    });
    {
        let wcap = "foo";
        let xcap = 97;
        let ycap = [ true, -1, "yup" ];
        let zcap = {
            a: "a",
            b: false,
            c: [ 0 ],
        };
        cases.push({
            title: "Serializes basic captures",
            func: () => { console.log(wcap + `${xcap}` + ycap.length + eval(zcap.a)); },
            expect: {
                code: "(() => { console.log(wcap + `${xcap}` + ycap.length + eval(zcap.a)); })",
                environment: {
                    wcap: {
                        json: "foo",
                    },
                    xcap: {
                        json: 97
                    },
                    ycap: {
                        arr: [
                            { json: true },
                            { json: -1 },
                            { json: "yup" },
                        ],
                    },
                    zcap: {
                        obj: {
                            a: { json: "a" },
                            b: { json: false },
                            c: { arr: [ { json: 0 } ] },
                        },
                    },
                },
                runtime: "nodejs",
            },
        });
    }
    {
        let nocap1 = 1, nocap2 = 2, nocap3 = 3, nocap4 = 4, nocap5 = 5;
        let cap1 = 100, cap2 = 200, cap3 = 300, cap4 = 400;
        let functext = `((nocap1, nocap2) => {
    let zz = nocap1 + nocap2; // not a capture: args
    let yy = nocap3; // not a capture: var later on
    if (zz) {
        zz += cap1; // true capture
        let cap1 = 9; // because let is properly scoped
        zz += nocap4; // not a capture
        var nocap4 = 7; // because var is function scoped
        zz += cap2; // true capture
        const cap2 = 33;
        var nocap3 = 8; // block the above capture
    }
    let f1 = (nocap5) => {
        yy += nocap5; // not a capture: args
        cap3++; // capture
    };
    let f2 = (function (nocap6) {
        zz += nocap6; // not a capture: args
        if (cap4) { // capture
            yy = 0;
        }
    });
})`;
        cases.push({
            title: "Doesn't serialize non-free variables (but retains frees)",
            func: eval(functext),
            expect: {
                code: functext,
                environment: {
                    cap1: { json: 100 },
                    cap2: { json: 200 },
                    cap3: { json: 300 },
                    cap4: { json: 400 },
                },
                runtime: "nodejs",
            },
        });
    }

    // Recursive function serialization.
    {
        let fff = "fff!";
        let ggg = "ggg!";
        let xcap = {
            fff: function () { console.log(fff); },
            ggg: () => { console.log(ggg); },
            zzz: {
                a: [ (a1: any, a2: any) => { console.log(a1 + a2); } ],
            },
        };
        let functext = `(() => {
    xcap.fff();
    xcap.ggg();
    xcap.zzz.a[0]("x", "y");
})`;
        cases.push({
            title: "Serializes recursive function captures",
            func: eval(functext),
            expect: {
                code: functext,
                environment: {
                    xcap: {
                        obj: {
                            fff: {
                                closure: {
                                    code: "(function () { console.log(fff); })",
                                    environment: { fff: { json: "fff!" } },
                                    runtime: "nodejs",
                                },
                            },
                            ggg: {
                                closure: {
                                    code: "(() => { console.log(ggg); })",
                                    environment: { ggg: { json: "ggg!" } },
                                    runtime: "nodejs",
                                },
                            },
                            zzz: {
                                obj: {
                                    a: {
                                        arr: [
                                            {
                                                closure: {
                                                    code: "((a1, a2) => { console.log(a1 + a2); })",
                                                    environment: {},
                                                    runtime: "nodejs",
                                                },
                                            },
                                        ],
                                    },
                                },
                            }
                        },
                    },
                },
                runtime: "nodejs",
            },
        });
    }

    // Now go ahead and run the test cases, each as its own case.
    for (let test of cases) {
        it(test.title, () => {
            if (test.expect) {
                assert.deepEqual(runtime.serializeClosure(test.func), test.expect);
            } else {
                assert.throws(() => runtime.serializeClosure(test.func));
            }
        });
    };
});


