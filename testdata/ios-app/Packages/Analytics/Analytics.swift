import MathKit

/// A dummy package that depends on MathKit, forming a small dependency chain.
public enum Analytics {
    public static func summarize(_ values: [Int]) -> Int {
        MathKit.sum(values)
    }

    public static func average(_ values: [Int]) -> Double {
        MathKit.mean(values)
    }
}
