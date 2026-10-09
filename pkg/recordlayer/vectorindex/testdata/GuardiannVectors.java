import com.apple.foundationdb.async.common.StorageTransform;
import com.apple.foundationdb.linear.*;
import com.apple.foundationdb.rabitq.RaBitQuantizer;
import java.util.HexFormat;

class GuardiannVectors {
    public static void main(String[] args) {
        System.out.println("half " + HexFormat.of().formatHex(new HalfRealVector(new double[] {0,1,-1,0.5,3.14159,-65504,65504,1e-8,123.456}).getRawData()));
        for (Metric metric : new Metric[] {Metric.EUCLIDEAN_METRIC, Metric.COSINE_METRIC, Metric.DOT_PRODUCT_METRIC, Metric.EUCLIDEAN_SQUARE_METRIC}) {
            var transform = new StorageTransform(new FhtKacRotator(42L, 3, 10),
                    new DoubleRealVector(new double[] {-0.25, 0.5, -0.75}), metric == Metric.COSINE_METRIC);
            var raw = new HalfRealVector(new double[] {1.0, 2.0, 3.0});
            System.out.println(metric + " normalized " + HexFormat.of().formatHex(raw.normalize().getRawData()));
            var vector = transform.transform(raw);
            System.out.println(metric + " transformed " + HexFormat.of().formatHex(vector.getUnderlyingVector().getRawData()));
            var quantizer = new RaBitQuantizer(metric, 4);
            var encoded = quantizer.encode(vector);
            System.out.println(metric + " encoded " + HexFormat.of().formatHex(encoded.getUnderlyingVector().getRawData()));
            System.out.println(metric + " returned " + HexFormat.of().formatHex(transform.untransform(encoded).getRawData()));
            var query = transform.transform(new DoubleRealVector(new double[] {1.5, 2.5, 3.5}));
            System.out.println(metric + " distance " + quantizer.estimator().distance(query, encoded));
        }
    }
}
