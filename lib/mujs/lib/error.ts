// Copyright 2016 Marapongo, Inc. All rights reserved.

// Error objects are thrown when ECMAScript runtime errors occur. The Error class can also be used as a base for user-
// defined exceptions. See https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Error/.
export class Error {
    public name: string;
    public message: string | undefined;
    constructor(message?: string) {
        this.name = "Error";
        this.message = message;
    }
}

// TODO[marapongo/mu#70]: consider projecting all of the various subclasses (EvalError, RangeError, ReferenceError,
//     SyntaxError, TypeError, etc.)  Unfortunately, unless we come up with some clever way of mapping MuIL runtime
//     errors into their ECMAScript equivalents, we aren't going to have perfect compatibility with error path logic.

