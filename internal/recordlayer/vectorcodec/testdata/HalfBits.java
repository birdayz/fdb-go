import com.apple.foundationdb.half.Half;

class HalfBits {
    public static void main(String[] args) {
        for (float value : new float[] {0, -0.0f, 1, -2, 65504, 65520, 1e10f, -1e10f,
                Float.POSITIVE_INFINITY, Float.NEGATIVE_INFINITY,
                5.960464477539063e-08f, 2.980232238769531e-08f, 8.940696716308594e-08f,
                6.103515625e-05f, 1f + 1f/2048, 1f + 3f/2048, Float.NaN}) {
            System.out.printf("%s %04x%n", value, Half.floatToShortBitsCollapseNaN(Half.valueOf(value).floatValue()));
        }
    }
}
