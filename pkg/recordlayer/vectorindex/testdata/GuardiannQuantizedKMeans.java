import com.apple.foundationdb.kmeans.KMeans;
import com.apple.foundationdb.kmeans.PartitionEvaluator;
import com.apple.foundationdb.linear.*;
import com.apple.foundationdb.rabitq.RaBitQuantizer;
import com.apple.foundationdb.util.Lens;
import java.util.*;

class GuardiannQuantizedKMeans {
    public static void main(String[] args) {
        var q = new RaBitQuantizer(Metric.EUCLIDEAN_METRIC, 4);
        List<RealVector> vectors = new ArrayList<>();
        for (int i = 0; i < 8; i++) {
            RealVector v = new DoubleRealVector(new double[] {i / 4 * 10.0, i % 4, 1});
            vectors.add(i < 3 ? v : q.encode(v));
        }
        for (int k : new int[] {1, 2}) {
            var r = KMeans.fit(new SplittableRandom(11), q.estimator(), Lens.<RealVector>identity(), Lens.<RealVector>identity(), vectors, k, 8, 3, 0, null);
            System.out.println(k + " " + r.objective() + " " + Arrays.toString(r.assignment()));
            for (var c : r.clusterCentroids()) System.out.println(Arrays.toString(c.getData()));
            if (k == 2) {
                var current = new PartitionEvaluator.Partition<RealVector>(List.of(new DoubleRealVector(new double[] {5,1.5,1})), Lens.identity(), new int[8]);
                var candidate = new PartitionEvaluator.Partition<RealVector>(r.clusterCentroids(), Lens.identity(), r.assignment());
                var e = PartitionEvaluator.evaluate(vectors, current, vectors, candidate, Lens.identity(), new PartitionEvaluator.Parameters(q.estimator()));
                System.out.println(e.decision() + " " + e.scoreGain());
            }
        }
    }
}
