import unittest

from trackfs.cuesheet import Time, Track
from trackfs.flactracks import TrackManager
from trackfs.fusepath import Factory, FusePath


class TimeTests(unittest.TestCase):
    def test_cue_frames_are_seventy_five_per_second(self):
        self.assertAlmostEqual(Time(0, 0, 75).seconds(), 1.0)


class FusePathTests(unittest.TestCase):
    def test_unicode_title_is_preserved_in_virtual_path(self):
        path = FusePath("album", ".flac", True, 1, "Beyoncé 日本語")
        self.assertIn("Beyoncé 日本語", path.vpath)

    def test_separator_and_extension_are_regex_escaped(self):
        factory = Factory(track_separator=".+", track_extension=".flac")
        path = factory.from_vpath("album.flac.+001.Title.flac")
        self.assertTrue(path.is_track)
        self.assertEqual(path.num, 1)


class TrackManagerTests(unittest.TestCase):
    def test_next_track_uses_position_not_track_number_as_index(self):
        first = Track(10, "A", start=Time(0, 0, 0), end=Time(0, 1, 0))
        second = Track(20, "A", start=Time(0, 1, 0), end=Time(0, 2, 0))

        class Album:
            def tracks(self):
                return [first, second]

        current, following = TrackManager._find_this_and_next_track(Album(), 10)
        self.assertIs(current, first)
        self.assertIs(following, second)

    def test_duration_is_compared_as_seconds(self):
        track = Track(1, "A", start=Time(0, 0, 0), end=Time(0, 0, 75))
        self.assertEqual(track.duration.seconds(), 1.0)

    def test_time_subtraction_normalizes_frame_boundary(self):
        self.assertEqual(Time(1, 0, 0) - Time(0, 0, 75), Time(0, 59, 0))


if __name__ == "__main__":
    unittest.main()
