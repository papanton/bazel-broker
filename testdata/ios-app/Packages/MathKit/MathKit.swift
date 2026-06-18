/// A dummy leaf package: simple integer math.
public enum MathKit {
    public static func sum(_ values: [Int]) -> Int {
        values.reduce(0, +)
    }

    public static func mean(_ values: [Int]) -> Double {
        guard !values.isEmpty else { return 0 }
        return Double(sum(values)) / Double(values.count)
    }
}
